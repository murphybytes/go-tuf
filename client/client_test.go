package client

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flynn/go-tuf"
	"github.com/flynn/go-tuf/data"
	"github.com/flynn/go-tuf/keys"
	"github.com/flynn/go-tuf/signed"
	"github.com/flynn/go-tuf/util"
	. "gopkg.in/check.v1"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) { TestingT(t) }

type ClientSuite struct {
	store       tuf.LocalStore
	repo        *tuf.Repo
	local       LocalStore
	remote      FakeRemoteStore
	expiredTime time.Time
	keyIDs      map[string]string
}

var _ = Suite(&ClientSuite{})

type FakeRemoteStore map[string]*fakeFile

func (f FakeRemoteStore) Get(path string) (io.ReadCloser, int64, error) {
	file, ok := f[path]
	if !ok {
		return nil, 0, ErrNotFound{strings.TrimPrefix(path, "targets/")}
	}
	return file, file.size, nil
}

func newFakeFile(b []byte) *fakeFile {
	return &fakeFile{buf: bytes.NewReader(b), size: int64(len(b))}
}

func newBlockingFakeFile(b []byte) *fakeFile {
	return &fakeFile{typ: "blocking", buf: bytes.NewReader(b), size: int64(len(b))}
}

func newSlowFakeFile(b []byte) *fakeFile {
	return &fakeFile{typ: "slow", buf: bytes.NewReader(b), size: int64(len(b))}
}

type fakeFile struct {
	typ       string
	buf       *bytes.Reader
	bytesRead int
	size      int64
}

func (f *fakeFile) Read(p []byte) (int, error) {
	switch f.typ {
	case "blocking":
		<-make(chan struct{})
	case "slow":
		time.Sleep(2 * time.Second)
	}
	n, err := f.buf.Read(p)
	f.bytesRead += n
	return n, err
}

func (f *fakeFile) Close() error {
	f.buf.Seek(0, os.SEEK_SET)
	return nil
}

var targetFiles = map[string][]byte{
	"foo.txt": []byte("foo"),
	"bar.txt": []byte("bar"),
	"baz.txt": []byte("baz"),
}

func (s *ClientSuite) SetUpTest(c *C) {
	s.store = tuf.MemoryStore(nil, targetFiles)

	// create a valid repo containing foo.txt
	var err error
	s.repo, err = tuf.NewRepo(s.store)
	c.Assert(err, IsNil)
	s.keyIDs = map[string]string{
		"root":      s.genKey(c, "root"),
		"targets":   s.genKey(c, "targets"),
		"snapshot":  s.genKey(c, "snapshot"),
		"timestamp": s.genKey(c, "timestamp"),
	}
	c.Assert(s.repo.AddTarget("foo.txt", nil), IsNil)
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)

	// create a remote store containing valid repo files
	s.remote = make(FakeRemoteStore)
	s.syncRemote(c)
	for k, v := range targetFiles {
		s.remote["targets/"+k] = newFakeFile(v)
	}

	s.expiredTime = time.Now().Add(time.Hour)
}

func (s *ClientSuite) genKey(c *C, role string) string {
	id, err := s.repo.GenKey(role)
	c.Assert(err, IsNil)
	return id
}

func (s *ClientSuite) genKeyExpired(c *C, role string) string {
	id, err := s.repo.GenKeyWithExpires(role, s.expiredTime)
	c.Assert(err, IsNil)
	return id
}

// withMetaExpired sets signed.IsExpired throughout the invocation of f so that
// any metadata marked to expire at s.expiredTime will be expired (this avoids
// the need to sleep in the tests).
func (s *ClientSuite) withMetaExpired(f func()) {
	e := signed.IsExpired
	signed.IsExpired = func(t time.Time) bool {
		return t.Unix() == s.expiredTime.Unix()
	}
	f()
	signed.IsExpired = e
}

func (s *ClientSuite) syncLocal(c *C) {
	meta, err := s.store.GetMeta()
	c.Assert(err, IsNil)
	for k, v := range meta {
		c.Assert(s.local.SetMeta(k, v), IsNil)
	}
}

func (s *ClientSuite) syncRemote(c *C) {
	meta, err := s.store.GetMeta()
	c.Assert(err, IsNil)
	for k, v := range meta {
		s.remote[k] = newFakeFile(v)
	}
}

func (s *ClientSuite) addRemoteTarget(c *C, name string) {
	c.Assert(s.repo.AddTarget(name, nil), IsNil)
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
}

func (s *ClientSuite) rootKeys(c *C) []*data.Key {
	rootKeys, err := s.repo.RootKeys()
	c.Assert(err, IsNil)
	c.Assert(rootKeys, HasLen, 1)
	return rootKeys
}

func (s *ClientSuite) newClient(c *C) *Client {
	s.local = MemoryLocalStore()
	client := NewClient(s.local, s.remote)
	c.Assert(client.Init(s.rootKeys(c), 1), IsNil)
	return client
}

func (s *ClientSuite) updatedClient(c *C) *Client {
	client := s.newClient(c)
	_, err := client.Update()
	c.Assert(err, IsNil)
	return client
}

func assertFiles(c *C, files data.Files, names []string) {
	c.Assert(files, HasLen, len(names))
	for _, name := range names {
		target, ok := targetFiles[name]
		if !ok {
			c.Fatalf("unknown target %s", name)
		}
		meta, err := util.GenerateFileMeta(bytes.NewReader(target))
		c.Assert(err, IsNil)
		file, ok := files[name]
		if !ok {
			c.Fatalf("expected files to contain %s", name)
		}
		c.Assert(util.FileMetaEqual(file, meta), IsNil)
	}
}

func assertWrongHash(c *C, err error) {
	// just test the type of err rather using DeepEquals as it contains
	// hashes we don't necessarily need to check.
	e, ok := err.(ErrDownloadFailed)
	if !ok {
		c.Fatalf("expected err to have type ErrDownloadFailed, got %T", err)
	}
	if _, ok := e.Err.(util.ErrWrongHash); !ok {
		c.Fatalf("expected err.Err to have type util.ErrWrongHash, got %T", err)
	}
}

func (s *ClientSuite) assertErrExpired(c *C, err error, file string) {
	decodeErr, ok := err.(ErrDecodeFailed)
	if !ok {
		c.Fatalf("expected err to have type ErrDecodeFailed, got %T", err)
	}
	c.Assert(decodeErr.File, Equals, file)
	expiredErr, ok := decodeErr.Err.(signed.ErrExpired)
	if !ok {
		c.Fatalf("expected err.Err to have type signed.ErrExpired, got %T", err)
	}
	c.Assert(expiredErr.Expired.Unix(), Equals, s.expiredTime.Unix())
}

func (s *ClientSuite) TestInitRootTooLarge(c *C) {
	client := NewClient(MemoryLocalStore(), s.remote)
	s.remote["root.json"] = newFakeFile(make([]byte, maxMetaSize+1))
	c.Assert(client.Init(s.rootKeys(c), 0), Equals, ErrMetaTooLarge{"root.json", maxMetaSize + 1})
}

func (s *ClientSuite) TestInitRootExpired(c *C) {
	s.genKeyExpired(c, "targets")
	s.syncRemote(c)
	client := NewClient(MemoryLocalStore(), s.remote)
	s.withMetaExpired(func() {
		s.assertErrExpired(c, client.Init(s.rootKeys(c), 1), "root.json")
	})
}

func (s *ClientSuite) TestInit(c *C) {
	client := NewClient(MemoryLocalStore(), s.remote)

	// check Init() returns keys.ErrInvalidThreshold with an invalid threshold
	c.Assert(client.Init(s.rootKeys(c), 0), Equals, keys.ErrInvalidThreshold)

	// check Init() returns signed.ErrRoleThreshold when not enough keys
	c.Assert(client.Init(s.rootKeys(c), 2), Equals, ErrInsufficientKeys)

	// check Update() returns ErrNoRootKeys when uninitialized
	_, err := client.Update()
	c.Assert(err, Equals, ErrNoRootKeys)

	// check Update() does not return ErrNoRootKeys after initialization
	c.Assert(client.Init(s.rootKeys(c), 1), IsNil)
	_, err = client.Update()
	c.Assert(err, Not(Equals), ErrNoRootKeys)
}

func (s *ClientSuite) TestFirstUpdate(c *C) {
	files, err := s.newClient(c).Update()
	c.Assert(err, IsNil)
	c.Assert(files, HasLen, 1)
	assertFiles(c, files, []string{"foo.txt"})
}

func (s *ClientSuite) TestMissingRemoteMetadata(c *C) {
	client := s.newClient(c)

	delete(s.remote, "targets.json")
	_, err := client.Update()
	c.Assert(err, Equals, ErrMissingRemoteMetadata{"targets.json"})

	delete(s.remote, "timestamp.json")
	_, err = client.Update()
	c.Assert(err, Equals, ErrMissingRemoteMetadata{"timestamp.json"})
}

func (s *ClientSuite) TestNoChangeUpdate(c *C) {
	client := s.newClient(c)
	_, err := client.Update()
	c.Assert(err, IsNil)
	_, err = client.Update()
	c.Assert(IsLatestSnapshot(err), Equals, true)
}

func (s *ClientSuite) TestNewTimestamp(c *C) {
	client := s.updatedClient(c)
	version := client.timestampVer
	c.Assert(version > 0, Equals, true)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	_, err := client.Update()
	c.Assert(IsLatestSnapshot(err), Equals, true)
	c.Assert(client.timestampVer > version, Equals, true)
}

func (s *ClientSuite) TestNewRoot(c *C) {
	client := s.newClient(c)

	// replace all keys
	newKeyIDs := make(map[string]string)
	for role, id := range s.keyIDs {
		c.Assert(s.repo.RevokeKey(role, id), IsNil)
		newKeyIDs[role] = s.genKey(c, role)
	}

	// update metadata
	c.Assert(s.repo.Sign("targets.json"), IsNil)
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)

	// check update gets new root version
	c.Assert(client.getLocalMeta(), IsNil)
	version := client.rootVer
	c.Assert(version > 0, Equals, true)
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.rootVer > version, Equals, true)

	// check old keys are not in db
	for _, id := range s.keyIDs {
		c.Assert(client.db.GetKey(id), IsNil)
	}

	// check new keys are in db
	for name, id := range newKeyIDs {
		key := client.db.GetKey(id)
		c.Assert(key, NotNil)
		c.Assert(key.ID, Equals, id)
		role := client.db.GetRole(name)
		c.Assert(role, NotNil)
		c.Assert(role.KeyIDs, DeepEquals, map[string]struct{}{id: {}})
	}
}

func (s *ClientSuite) TestNewTargets(c *C) {
	client := s.newClient(c)
	files, err := client.Update()
	c.Assert(err, IsNil)
	assertFiles(c, files, []string{"foo.txt"})

	s.addRemoteTarget(c, "bar.txt")
	s.addRemoteTarget(c, "baz.txt")

	files, err = client.Update()
	c.Assert(err, IsNil)
	assertFiles(c, files, []string{"bar.txt", "baz.txt"})

	// Adding the same exact file should not lead to an update
	s.addRemoteTarget(c, "bar.txt")
	files, err = client.Update()
	c.Assert(err, IsNil)
	c.Assert(files, HasLen, 0)
}

func (s *ClientSuite) TestNewTimestampKey(c *C) {
	client := s.newClient(c)

	// replace key
	oldID := s.keyIDs["timestamp"]
	c.Assert(s.repo.RevokeKey("timestamp", oldID), IsNil)
	newID := s.genKey(c, "timestamp")

	// generate new snapshot (because root has changed) and timestamp
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)

	// check update gets new root and timestamp
	c.Assert(client.getLocalMeta(), IsNil)
	rootVer := client.rootVer
	timestampVer := client.timestampVer
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.rootVer > rootVer, Equals, true)
	c.Assert(client.timestampVer > timestampVer, Equals, true)

	// check key has been replaced in db
	c.Assert(client.db.GetKey(oldID), IsNil)
	key := client.db.GetKey(newID)
	c.Assert(key, NotNil)
	c.Assert(key.ID, Equals, newID)
	role := client.db.GetRole("timestamp")
	c.Assert(role, NotNil)
	c.Assert(role.KeyIDs, DeepEquals, map[string]struct{}{newID: {}})
}

func (s *ClientSuite) TestNewSnapshotKey(c *C) {
	client := s.newClient(c)

	// replace key
	oldID := s.keyIDs["snapshot"]
	c.Assert(s.repo.RevokeKey("snapshot", oldID), IsNil)
	newID := s.genKey(c, "snapshot")

	// generate new snapshot and timestamp
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)

	// check update gets new root, snapshot and timestamp
	c.Assert(client.getLocalMeta(), IsNil)
	rootVer := client.rootVer
	snapshotVer := client.snapshotVer
	timestampVer := client.timestampVer
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.rootVer > rootVer, Equals, true)
	c.Assert(client.snapshotVer > snapshotVer, Equals, true)
	c.Assert(client.timestampVer > timestampVer, Equals, true)

	// check key has been replaced in db
	c.Assert(client.db.GetKey(oldID), IsNil)
	key := client.db.GetKey(newID)
	c.Assert(key, NotNil)
	c.Assert(key.ID, Equals, newID)
	role := client.db.GetRole("snapshot")
	c.Assert(role, NotNil)
	c.Assert(role.KeyIDs, DeepEquals, map[string]struct{}{newID: {}})
}

func (s *ClientSuite) TestNewTargetsKey(c *C) {
	client := s.newClient(c)

	// replace key
	oldID := s.keyIDs["targets"]
	c.Assert(s.repo.RevokeKey("targets", oldID), IsNil)
	newID := s.genKey(c, "targets")

	// re-sign targets and generate new snapshot and timestamp
	c.Assert(s.repo.Sign("targets.json"), IsNil)
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)

	// check update gets new metadata
	c.Assert(client.getLocalMeta(), IsNil)
	rootVer := client.rootVer
	targetsVer := client.targetsVer
	snapshotVer := client.snapshotVer
	timestampVer := client.timestampVer
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.rootVer > rootVer, Equals, true)
	c.Assert(client.targetsVer > targetsVer, Equals, true)
	c.Assert(client.snapshotVer > snapshotVer, Equals, true)
	c.Assert(client.timestampVer > timestampVer, Equals, true)

	// check key has been replaced in db
	c.Assert(client.db.GetKey(oldID), IsNil)
	key := client.db.GetKey(newID)
	c.Assert(key, NotNil)
	c.Assert(key.ID, Equals, newID)
	role := client.db.GetRole("targets")
	c.Assert(role, NotNil)
	c.Assert(role.KeyIDs, DeepEquals, map[string]struct{}{newID: {}})
}

func (s *ClientSuite) TestLocalExpired(c *C) {
	client := s.newClient(c)

	// locally expired timestamp.json is ok
	version := client.timestampVer
	c.Assert(s.repo.TimestampWithExpires(s.expiredTime), IsNil)
	s.syncLocal(c)
	s.withMetaExpired(func() {
		c.Assert(client.getLocalMeta(), IsNil)
		c.Assert(client.timestampVer > version, Equals, true)
	})

	// locally expired snapshot.json is ok
	version = client.snapshotVer
	c.Assert(s.repo.SnapshotWithExpires(tuf.CompressionTypeNone, s.expiredTime), IsNil)
	s.syncLocal(c)
	s.withMetaExpired(func() {
		c.Assert(client.getLocalMeta(), IsNil)
		c.Assert(client.snapshotVer > version, Equals, true)
	})

	// locally expired targets.json is ok
	version = client.targetsVer
	c.Assert(s.repo.AddTargetWithExpires("foo.txt", nil, s.expiredTime), IsNil)
	s.syncLocal(c)
	s.withMetaExpired(func() {
		c.Assert(client.getLocalMeta(), IsNil)
		c.Assert(client.targetsVer > version, Equals, true)
	})

	// locally expired root.json is not ok
	version = client.rootVer
	s.genKeyExpired(c, "targets")
	s.syncLocal(c)
	s.withMetaExpired(func() {
		err := client.getLocalMeta()
		if _, ok := err.(signed.ErrExpired); !ok {
			c.Fatalf("expected err to have type signed.ErrExpired, got %T", err)
		}
		c.Assert(client.rootVer, Equals, version)
	})
}

func (s *ClientSuite) TestTimestampTooLarge(c *C) {
	s.remote["timestamp.json"] = newFakeFile(make([]byte, maxMetaSize+1))
	_, err := s.newClient(c).Update()
	c.Assert(err, Equals, ErrMetaTooLarge{"timestamp.json", maxMetaSize + 1})
}

func (s *ClientSuite) TestUpdateLocalRootExpired(c *C) {
	client := s.newClient(c)

	// add soon to expire root.json to local storage
	s.genKeyExpired(c, "timestamp")
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncLocal(c)

	// add far expiring root.json to remote storage
	s.genKey(c, "timestamp")
	s.addRemoteTarget(c, "bar.txt")
	s.syncRemote(c)

	// check the update downloads the non expired remote root.json and
	// restarts itself, thus successfully updating
	s.withMetaExpired(func() {
		err := client.getLocalMeta()
		if _, ok := err.(signed.ErrExpired); !ok {
			c.Fatalf("expected err to have type signed.ErrExpired, got %T", err)
		}
		_, err = client.Update()
		c.Assert(err, IsNil)
	})
}

func (s *ClientSuite) TestUpdateRemoteExpired(c *C) {
	client := s.updatedClient(c)

	// expired remote metadata should always be rejected
	c.Assert(s.repo.TimestampWithExpires(s.expiredTime), IsNil)
	s.syncRemote(c)
	s.withMetaExpired(func() {
		_, err := client.Update()
		s.assertErrExpired(c, err, "timestamp.json")
	})

	c.Assert(s.repo.SnapshotWithExpires(tuf.CompressionTypeNone, s.expiredTime), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	s.withMetaExpired(func() {
		_, err := client.Update()
		s.assertErrExpired(c, err, "snapshot.json")
	})

	c.Assert(s.repo.AddTargetWithExpires("bar.txt", nil, s.expiredTime), IsNil)
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	s.withMetaExpired(func() {
		_, err := client.Update()
		s.assertErrExpired(c, err, "targets.json")
	})

	s.genKeyExpired(c, "timestamp")
	c.Assert(s.repo.RemoveTarget("bar.txt"), IsNil)
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	s.withMetaExpired(func() {
		_, err := client.Update()
		s.assertErrExpired(c, err, "root.json")
	})
}

func (s *ClientSuite) TestUpdateMixAndMatchAttack(c *C) {
	client := s.updatedClient(c)

	// grab the remote targets.json
	oldTargets, ok := s.remote["targets.json"]
	if !ok {
		c.Fatal("missing remote targets.json")
	}

	// generate new remote metadata, but replace targets.json with the old one
	s.addRemoteTarget(c, "bar.txt")
	newTargets, ok := s.remote["targets.json"]
	if !ok {
		c.Fatal("missing remote targets.json")
	}
	s.remote["targets.json"] = oldTargets

	// check update returns ErrWrongSize for targets.json
	_, err := client.Update()
	c.Assert(err, DeepEquals, ErrWrongSize{"targets.json", oldTargets.size, newTargets.size})

	// do the same but keep the size the same
	c.Assert(s.repo.RemoveTarget("foo.txt"), IsNil)
	c.Assert(s.repo.Snapshot(tuf.CompressionTypeNone), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	s.remote["targets.json"] = oldTargets

	// check update returns ErrWrongHash
	_, err = client.Update()
	assertWrongHash(c, err)
}

func (s *ClientSuite) TestUpdateReplayAttack(c *C) {
	client := s.updatedClient(c)

	// grab the remote timestamp.json
	oldTimestamp, ok := s.remote["timestamp.json"]
	if !ok {
		c.Fatal("missing remote timestamp.json")
	}

	// generate a new timestamp and sync with the client
	version := client.timestampVer
	c.Assert(version > 0, Equals, true)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	_, err := client.Update()
	c.Assert(IsLatestSnapshot(err), Equals, true)
	c.Assert(client.timestampVer > version, Equals, true)

	// replace remote timestamp.json with the old one
	s.remote["timestamp.json"] = oldTimestamp

	// check update returns ErrLowVersion
	_, err = client.Update()
	c.Assert(err, DeepEquals, ErrDecodeFailed{"timestamp.json", signed.ErrLowVersion{version, client.timestampVer}})
}

func (s *ClientSuite) TestUpdateTamperedTargets(c *C) {
	client := s.newClient(c)

	// get local targets.json
	meta, err := s.store.GetMeta()
	c.Assert(err, IsNil)
	targetsJSON, ok := meta["targets.json"]
	if !ok {
		c.Fatal("missing targets.json")
	}
	targets := &data.Signed{}
	c.Assert(json.Unmarshal(targetsJSON, targets), IsNil)

	// update remote targets.json to have different content but same size
	c.Assert(targets.Signatures, HasLen, 1)
	targets.Signatures[0].Method = "xxxxxxx"
	tamperedJSON, err := json.Marshal(targets)
	c.Assert(err, IsNil)
	s.store.SetMeta("targets.json", tamperedJSON)
	s.syncRemote(c)
	_, err = client.Update()
	assertWrongHash(c, err)

	// update remote targets.json to have the wrong size
	targets.Signatures[0].Method = "xxx"
	tamperedJSON, err = json.Marshal(targets)
	c.Assert(err, IsNil)
	s.store.SetMeta("targets.json", tamperedJSON)
	s.syncRemote(c)
	_, err = client.Update()
	c.Assert(err, DeepEquals, ErrWrongSize{"targets.json", int64(len(tamperedJSON)), int64(len(targetsJSON))})
}

func (s *ClientSuite) TestUpdateSlowRetrievalAttack(c *C) {
	meta, err := s.store.GetMeta()
	c.Assert(err, IsNil)
	snapshot, ok := meta["snapshot.json"]
	if !ok {
		c.Fatal("missing snapshot.json")
	}
	client := s.newClient(c)

	s.remote["snapshot.json"] = newBlockingFakeFile(snapshot)
	_, err = client.Update()
	// c.Assert(err, DeepEquals, ErrWrongSize{"snapshot.json", 0, int64(len(snapshot))})
	c.Assert(err, DeepEquals, ErrDownloadFailed{"snapshot.json", util.ErrWrongLength})

	s.remote["snapshot.json"] = newSlowFakeFile(snapshot)
	_, err = client.Update()
	// c.Assert(err, DeepEquals, ErrWrongSize{"snapshot.json", 16384, int64(len(snapshot))})
	c.Assert(err, DeepEquals, ErrDownloadFailed{"snapshot.json", util.ErrWrongLength})
}

type testDestination struct {
	bytes.Buffer
	deleted bool
}

func (t *testDestination) Delete() error {
	t.deleted = true
	return nil
}

func (s *ClientSuite) TestDownloadUnknownTarget(c *C) {
	client := s.updatedClient(c)
	var dest testDestination
	c.Assert(client.Download("nonexistent", &dest), Equals, ErrUnknownTarget{"nonexistent"})
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestDownloadNoExist(c *C) {
	client := s.updatedClient(c)
	delete(s.remote, "targets/foo.txt")
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), Equals, ErrNotFound{"foo.txt"})
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestDownloadOK(c *C) {
	client := s.updatedClient(c)
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), IsNil)
	c.Assert(dest.deleted, Equals, false)
	c.Assert(dest.String(), Equals, "foo")
}

func (s *ClientSuite) TestDownloadWrongSize(c *C) {
	client := s.updatedClient(c)
	remoteFile := &fakeFile{buf: bytes.NewReader([]byte("wrong-size")), size: 10}
	s.remote["targets/foo.txt"] = remoteFile
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), DeepEquals, ErrWrongSize{"foo.txt", 10, 3})
	c.Assert(remoteFile.bytesRead, Equals, 0)
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestDownloadTargetTooLong(c *C) {
	client := s.updatedClient(c)
	remoteFile := s.remote["targets/foo.txt"]
	remoteFile.buf = bytes.NewReader([]byte("foo-ooo"))
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), IsNil)
	c.Assert(remoteFile.bytesRead, Equals, 3)
	c.Assert(dest.deleted, Equals, false)
	c.Assert(dest.String(), Equals, "foo")
}

func (s *ClientSuite) TestDownloadTargetTooShort(c *C) {
	client := s.updatedClient(c)
	remoteFile := s.remote["targets/foo.txt"]
	remoteFile.buf = bytes.NewReader([]byte("fo"))
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), DeepEquals, ErrWrongSize{"foo.txt", 2, 3})
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestDownloadTargetCorruptData(c *C) {
	client := s.updatedClient(c)
	remoteFile := s.remote["targets/foo.txt"]
	remoteFile.buf = bytes.NewReader([]byte("corrupt"))
	var dest testDestination
	assertWrongHash(c, client.Download("foo.txt", &dest))
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestAvailableTargets(c *C) {
	client := s.updatedClient(c)
	files, err := client.Targets()
	c.Assert(err, IsNil)
	assertFiles(c, files, []string{"foo.txt"})

	s.addRemoteTarget(c, "bar.txt")
	s.addRemoteTarget(c, "baz.txt")
	_, err = client.Update()
	c.Assert(err, IsNil)
	files, err = client.Targets()
	c.Assert(err, IsNil)
	assertFiles(c, files, []string{"foo.txt", "bar.txt", "baz.txt"})
}
