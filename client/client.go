package client

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"

	"github.com/flynn/go-tuf/data"
	"github.com/flynn/go-tuf/keys"
	"github.com/flynn/go-tuf/signed"
	"github.com/flynn/go-tuf/util"
)

// LocalStore is local storage for downloaded top-level metadata.
type LocalStore interface {
	// GetMeta returns top-level metadata from local storage. The keys are
	// in the form `ROLE.json`, with ROLE being a valid top-level role.
	GetMeta() (map[string]json.RawMessage, error)

	// SetMeta persists the given top-level metadata in local storage, the
	// name taking the same format as the keys returned by GetMeta.
	SetMeta(name string, meta json.RawMessage) error
}

// RemoteStore downloads top-level metadata and target files from a remote
// repository.
type RemoteStore interface {
	// Get downloads the given file from remote storage.
	//
	// `path` is the path of the file relative to the root of the remote
	//        repository (e.g. "root.json" or "targets/path/to/file.txt").
	//
	// `err` is ErrNotFound if the given file does not exist.
	//
	// `size` is the size of the stream, -1 indicating an unknown length.
	Get(path string) (stream io.ReadCloser, size int64, err error)
}

// Client provides methods for fetching updates from a remote repository and
// downloading remote target files.
type Client struct {
	local  LocalStore
	remote RemoteStore

	// The following four fields represent the versions of metatdata either
	// from local storage or from recently downloaded metadata
	rootVer      int
	targetsVer   int
	snapshotVer  int
	timestampVer int

	// targets is the list of available targets, either from local storage
	// or from recently downloaded targets metadata
	targets data.Files

	// localMeta is the raw metadata from local storage and is used to
	// check whether remote metadata is present locally
	localMeta map[string]json.RawMessage

	// db is a key DB used for verifying metadata
	db *keys.DB
}

func NewClient(local LocalStore, remote RemoteStore) *Client {
	return &Client{
		local:  local,
		remote: remote,
	}
}

// Init initializes a local repository.
//
// The latest root.json is fetched from remote storage, verified using rootKeys
// and threshold, and then saved in local storage. It is expected that rootKeys
// were securely distributed with the software being updated.
func (c *Client) Init(rootKeys []*data.Key, threshold int) error {
	if len(rootKeys) < threshold {
		return ErrInsufficientKeys
	}
	rootJSON, err := c.downloadMetaUnsafe("root.json")
	if err != nil {
		return err
	}

	c.db = keys.NewDB()
	rootKeyIDs := make([]string, len(rootKeys))
	for i, key := range rootKeys {
		id := key.ID()
		rootKeyIDs[i] = id
		if err := c.db.AddKey(id, key); err != nil {
			return err
		}
	}
	role := &data.Role{Threshold: threshold, KeyIDs: rootKeyIDs}
	if err := c.db.AddRole("root", role); err != nil {
		return err
	}

	if err := c.decodeRoot(rootJSON); err != nil {
		return err
	}

	return c.local.SetMeta("root.json", rootJSON)
}

// Update downloads and verifies remote metadata and returns updated targets.
//
// It performs the update part of "The client application" workflow from
// section 5.1 of the TUF spec:
//
// https://github.com/theupdateframework/tuf/blob/v0.9.9/docs/tuf-spec.txt#L714
func (c *Client) Update() (data.Files, error) {
	return c.update(false)
}

func (c *Client) update(latestRoot bool) (data.Files, error) {
	// Always start the update using local metadata
	if err := c.getLocalMeta(); err != nil {
		if _, ok := err.(signed.ErrExpired); ok {
			if !latestRoot {
				return c.updateWithLatestRoot(nil)
			}
			// this should not be reached as if the latest root has
			// been downloaded and it is expired, updateWithLatestRoot
			// should not have continued the update
			return nil, err
		}
		return nil, err
	}

	// Get timestamp.json, extract snapshot.json file meta and save the
	// timestamp.json locally
	timestampJSON, err := c.downloadMetaUnsafe("timestamp.json")
	if err != nil {
		return nil, err
	}
	snapshotMeta, err := c.decodeTimestamp(timestampJSON)
	if err != nil {
		// ErrRoleThreshold could indicate timestamp keys have been
		// revoked, so retry with the latest root.json
		if isDecodeFailedWithErr(err, signed.ErrRoleThreshold) && !latestRoot {
			return c.updateWithLatestRoot(nil)
		}
		return nil, err
	}
	if err := c.local.SetMeta("timestamp.json", timestampJSON); err != nil {
		return nil, err
	}

	// Return ErrLatestSnapshot if we already have the latest snapshot.json
	if c.hasMeta("snapshot.json", snapshotMeta) {
		return nil, ErrLatestSnapshot{c.snapshotVer}
	}

	// Get snapshot.json, then extract root.json and targets.json file meta.
	//
	// The snapshot.json is only saved locally after checking root.json and
	// targets.json so that it will be re-downloaded on subsequent updates
	// if this update fails.
	snapshotJSON, err := c.downloadMeta("snapshot.json", snapshotMeta)
	if err != nil {
		return nil, err
	}
	rootMeta, targetsMeta, err := c.decodeSnapshot(snapshotJSON)
	if err != nil {
		// ErrRoleThreshold could indicate snapshot keys have been
		// revoked, so retry with the latest root.json
		if isDecodeFailedWithErr(err, signed.ErrRoleThreshold) && !latestRoot {
			return c.updateWithLatestRoot(nil)
		}
		return nil, err
	}

	// If we don't have the root.json, download it, save it in local
	// storage and restart the update
	if !c.hasMeta("root.json", rootMeta) {
		return c.updateWithLatestRoot(&rootMeta)
	}

	// If we don't have the targets.json, download it, determine updated
	// targets and save targets.json in local storage
	var updatedTargets data.Files
	if !c.hasMeta("targets.json", targetsMeta) {
		targetsJSON, err := c.downloadMeta("targets.json", targetsMeta)
		if err != nil {
			return nil, err
		}
		updatedTargets, err = c.decodeTargets(targetsJSON)
		if err != nil {
			return nil, err
		}
		if err := c.local.SetMeta("targets.json", targetsJSON); err != nil {
			return nil, err
		}
	}

	// Save the snapshot.json now it has been processed successfully
	if err := c.local.SetMeta("snapshot.json", snapshotJSON); err != nil {
		return nil, err
	}

	return updatedTargets, nil
}

func (c *Client) updateWithLatestRoot(m *data.FileMeta) (data.Files, error) {
	var rootJSON json.RawMessage
	var err error
	if m == nil {
		rootJSON, err = c.downloadMetaUnsafe("root.json")
	} else {
		rootJSON, err = c.downloadMeta("root.json", *m)
	}
	if err != nil {
		return nil, err
	}
	if err := c.decodeRoot(rootJSON); err != nil {
		return nil, err
	}
	if err := c.local.SetMeta("root.json", rootJSON); err != nil {
		return nil, err
	}
	return c.update(true)
}

// getLocalMeta decodes and verifies metadata from local storage.
//
// The verification of local files is purely for consistency, if an attacker
// has compromised the local storage, there is no guarantee it can be trusted.
func (c *Client) getLocalMeta() error {
	meta, err := c.local.GetMeta()
	if err != nil {
		return err
	}

	if rootJSON, ok := meta["root.json"]; ok {
		// unmarshal root.json without verifying as we need the root
		// keys first
		s := &data.Signed{}
		if err := json.Unmarshal(rootJSON, s); err != nil {
			return err
		}
		root := &data.Root{}
		if err := json.Unmarshal(s.Signed, root); err != nil {
			return err
		}
		db := keys.NewDB()
		for id, k := range root.Keys {
			if err := db.AddKey(id, k); err != nil {
				return err
			}
		}
		for name, role := range root.Roles {
			if err := db.AddRole(name, role); err != nil {
				return err
			}
		}
		if err := signed.Verify(s, "root", 0, db); err != nil {
			return err
		}
		c.db = db
	} else {
		return ErrNoRootKeys
	}

	if snapshotJSON, ok := meta["snapshot.json"]; ok {
		snapshot := &data.Snapshot{}
		if err := signed.UnmarshalTrusted(snapshotJSON, snapshot, "snapshot", c.db); err != nil {
			return err
		}
		c.snapshotVer = snapshot.Version
	}

	if targetsJSON, ok := meta["targets.json"]; ok {
		targets := &data.Targets{}
		if err := signed.UnmarshalTrusted(targetsJSON, targets, "targets", c.db); err != nil {
			return err
		}
		c.targetsVer = targets.Version
		c.targets = targets.Targets
	}

	if timestampJSON, ok := meta["timestamp.json"]; ok {
		timestamp := &data.Timestamp{}
		if err := signed.UnmarshalTrusted(timestampJSON, timestamp, "timestamp", c.db); err != nil {
			return err
		}
		c.timestampVer = timestamp.Version
	}

	c.localMeta = meta
	return nil
}

// maxMetaSize is the maximum number of bytes that will be downloaded when
// getting remote metadata without knowing it's length.
const maxMetaSize = 50 * 1024

// downloadMetaUnsafe downloads top-level metadata from remote storage without
// verifying it's length and hashes (used for example to download timestamp.json
// which has unknown size). It will download at most maxMetaSize bytes.
func (c *Client) downloadMetaUnsafe(name string) ([]byte, error) {
	r, size, err := c.remote.Get(name)
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrMissingRemoteMetadata{name}
		}
		return nil, ErrDownloadFailed{name, err}
	}
	defer r.Close()

	// wrap in a timeoutReader to prevent slow retrieval attacks
	stream := newTimeoutReader(r)

	// return ErrMetaTooLarge if the reported size is greater than maxMetaSize
	if size > maxMetaSize {
		return nil, ErrMetaTooLarge{name, size}
	}

	// although the size has been checked above, use a LimitReader in case
	// the reported size is inaccurate, or size is -1 which indicates an
	// unknown length
	return ioutil.ReadAll(io.LimitReader(stream, maxMetaSize))
}

// downloadMeta downloads top-level metadata from remote storage and verifies
// it using the given file metadata.
func (c *Client) downloadMeta(name string, m data.FileMeta) ([]byte, error) {
	r, size, err := c.remote.Get(name)
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrMissingRemoteMetadata{name}
		}
		return nil, err
	}
	defer r.Close()

	// return ErrWrongSize if the reported size is known and incorrect
	if size >= 0 && size != m.Length {
		return nil, ErrWrongSize{name, size, m.Length}
	}

	// wrap the data in a timeoutReader to prevent slow retrieval attacks,
	// and a LimitReader so we download at most m.Length bytes
	stream := newTimeoutReader(io.LimitReader(r, m.Length))

	// read the data, simultaneously writing it to buf and generating metadata
	var buf bytes.Buffer
	meta, err := util.GenerateFileMeta(io.TeeReader(stream, &buf))
	if err != nil {
		return nil, err
	}
	if err := util.FileMetaEqual(meta, m); err != nil {
		return nil, ErrDownloadFailed{name, err}
	}
	return buf.Bytes(), nil
}

// decodeRoot decodes and verifies root metadata.
func (c *Client) decodeRoot(b json.RawMessage) error {
	root := &data.Root{}
	if err := signed.Unmarshal(b, root, "root", c.rootVer, c.db); err != nil {
		return ErrDecodeFailed{"root.json", err}
	}
	c.rootVer = root.Version
	return nil
}

// decodeSnapshot decodes and verifies snapshot metadata, and returns the new
// root and targets file meta.
func (c *Client) decodeSnapshot(b json.RawMessage) (data.FileMeta, data.FileMeta, error) {
	snapshot := &data.Snapshot{}
	if err := signed.Unmarshal(b, snapshot, "snapshot", c.snapshotVer, c.db); err != nil {
		return data.FileMeta{}, data.FileMeta{}, ErrDecodeFailed{"snapshot.json", err}
	}
	c.snapshotVer = snapshot.Version
	return snapshot.Meta["root.json"], snapshot.Meta["targets.json"], nil
}

// decodeTargets decodes and verifies targets metadata, sets c.targets and
// returns updated targets.
func (c *Client) decodeTargets(b json.RawMessage) (data.Files, error) {
	targets := &data.Targets{}
	if err := signed.Unmarshal(b, targets, "targets", c.targetsVer, c.db); err != nil {
		return nil, ErrDecodeFailed{"targets.json", err}
	}
	updatedTargets := make(data.Files)
	for path, meta := range targets.Targets {
		if local, ok := c.targets[path]; ok {
			if err := util.FileMetaEqual(local, meta); err == nil {
				continue
			}
		}
		updatedTargets[path] = meta
	}
	c.targetsVer = targets.Version
	c.targets = targets.Targets
	return updatedTargets, nil
}

// decodeTimestamp decodes and verifies timestamp metadata, and returns the
// new snapshot file meta.
func (c *Client) decodeTimestamp(b json.RawMessage) (data.FileMeta, error) {
	timestamp := &data.Timestamp{}
	if err := signed.Unmarshal(b, timestamp, "timestamp", c.timestampVer, c.db); err != nil {
		return data.FileMeta{}, ErrDecodeFailed{"timestamp.json", err}
	}
	c.timestampVer = timestamp.Version
	return timestamp.Meta["snapshot.json"], nil
}

// hasMeta checks whether local metadata has the given file meta
func (c *Client) hasMeta(name string, m data.FileMeta) bool {
	b, ok := c.localMeta[name]
	if !ok {
		return false
	}
	meta, err := util.GenerateFileMeta(bytes.NewReader(b))
	if err != nil {
		return false
	}
	err = util.FileMetaEqual(meta, m)
	return err == nil
}

type Destination interface {
	io.Writer
	Delete() error
}

// Download downloads the given target file from remote storage into dest.
//
// dest will be deleted and an error returned in the following situations:
//
//   * The target does not exist in the local targets.json
//   * The target does not exist in remote storage
//   * Metadata cannot be generated for the downloaded data
//   * Generated metadata does not match local metadata for the given file
func (c *Client) Download(name string, dest Destination) (err error) {
	// delete dest if there is an error
	defer func() {
		if err != nil {
			dest.Delete()
		}
	}()

	// populate c.targets from local storage if not set
	if c.targets == nil {
		if err := c.getLocalMeta(); err != nil {
			return err
		}
	}

	// return ErrNotFound if the file is not in the local targets.json
	localMeta, ok := c.targets[name]
	if !ok {
		return ErrUnknownTarget{name}
	}

	// get the data from remote storage
	r, size, err := c.remote.Get("targets/" + name)
	if err != nil {
		return err
	}
	defer r.Close()

	// return ErrWrongSize if the reported size is known and incorrect
	if size >= 0 && size != localMeta.Length {
		return ErrWrongSize{name, size, localMeta.Length}
	}

	// wrap the data in a timeoutReader to prevent slow retrieval attacks,
	// and a LimitReader so we download at most localMeta.Length bytes
	stream := newTimeoutReader(io.LimitReader(r, localMeta.Length))

	// read the data, simultaneously writing it to dest and generating metadata
	actual, err := util.GenerateFileMeta(io.TeeReader(stream, dest))
	if err != nil {
		return ErrDownloadFailed{name, err}
	}

	// check the data has the correct length and hashes
	if err := util.FileMetaEqual(actual, localMeta); err != nil {
		if err == util.ErrWrongLength {
			return ErrWrongSize{name, actual.Length, localMeta.Length}
		}
		return ErrDownloadFailed{name, err}
	}

	return nil
}

// Targets returns the complete list of available targets.
func (c *Client) Targets() (data.Files, error) {
	// populate c.targets from local storage if not set
	if c.targets == nil {
		if err := c.getLocalMeta(); err != nil {
			return nil, err
		}
	}
	return c.targets, nil
}
