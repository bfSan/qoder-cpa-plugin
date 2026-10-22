//go:build unix

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Adapted from WorkBuddy's openat/no-follow, pinned-directory, exclusive-process
// store. Qoder adds authenticated records and rejects symlinks in ALL ancestors.
// The MAC detects record edits/substitution; a same-UID attacker able to read the
// private key is outside this local-storage trust boundary.
func openOAuthPendingStore(dir string) (*oauthPendingStore, error) {
	parent, err := openOAuthDirectory(filepath.Dir(dir))
	if err != nil {
		return nil, errOAuthStorage
	}
	defer unix.Close(parent)
	name := filepath.Base(dir)
	if err := unix.Mkdirat(parent, name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, errOAuthStorage
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errOAuthStorage
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&07777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		unix.Close(fd)
		return nil, errOAuthStorage
	}
	lockFD, err := unix.Openat(fd, ".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		unix.Close(fd)
		return nil, errOAuthStorage
	}
	lock := os.NewFile(uintptr(lockFD), "oauth-pending-lock")
	if !safeOAuthFile(lockFD, 0600) || unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB) != nil {
		lock.Close()
		unix.Close(fd)
		return nil, errOAuthStorage
	}
	store := &oauthPendingStore{dir: dir, fd: fd, lock: lock}
	if err := store.loadKey(); err != nil {
		store.close()
		return nil, errOAuthStorage
	}
	if count, err := store.scan(time.Now()); err != nil || count > oauthPendingMaxFiles {
		store.close()
		return nil, errOAuthStorage
	}
	return store, nil
}

func openOAuthDirectory(dir string) (int, error) {
	if !filepath.IsAbs(dir) {
		return -1, errOAuthStorage
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(filepath.Clean(dir), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func safeOAuthFile(fd int, mode uint32) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&07777 == mode && st.Nlink == 1 && st.Uid == uint32(os.Geteuid())
}

func (s *oauthPendingStore) close() {
	if s.lock != nil {
		_ = s.lock.Close()
	}
	_ = unix.Close(s.fd)
}

func (s *oauthPendingStore) loadKey() error {
	fd, err := unix.Openat(s.fd, ".key", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		// Never generate a replacement key for existing records after key loss.
		scanFD, e := unix.Openat(s.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if e != nil {
			return errOAuthStorage
		}
		dir := os.NewFile(uintptr(scanFD), "oauth-key-directory")
		entries, e := dir.ReadDir(oauthPendingMaxFiles*2 + 2)
		dir.Close()
		if e != nil && e != io.EOF {
			return errOAuthStorage
		}
		for _, entry := range entries {
			if entry.Name() != ".lock" && !strings.HasPrefix(entry.Name(), ".tmp-") {
				return errOAuthStorage
			}
		}
		key := make([]byte, 32)
		if _, e := rand.Read(key); e != nil {
			return errOAuthStorage
		}
		// Exclusive-process lock prevents readers seeing the temporary key.
		if e := s.atomicWrite(".key", key); e != nil {
			return e
		}
		s.key = key
		return nil
	}
	if err != nil {
		return errOAuthStorage
	}
	f := os.NewFile(uintptr(fd), "oauth-pending-key")
	defer f.Close()
	if !safeOAuthFile(fd, 0600) {
		return errOAuthStorage
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		return errOAuthStorage
	}
	s.key = key
	return nil
}

type oauthSignedRecord struct {
	Record json.RawMessage `json:"record"`
	MAC    string          `json:"mac"`
}

func strictOAuthJSON(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errOAuthStorage
	}
	if dec.Decode(new(any)) != io.EOF {
		return errOAuthStorage
	}
	return nil
}

func (s *oauthPendingStore) read(name string) (*oauthPendingRecord, error) {
	if !validOAuthPendingName(name) {
		return nil, errOAuthStorage
	}
	fd, err := unix.Openat(s.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "oauth-pending-record")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !safeOAuthFile(fd, 0600) || st.Size() > oauthPendingMaxBytes {
		return nil, errOAuthStorage
	}
	raw, err := io.ReadAll(io.LimitReader(f, oauthPendingMaxBytes+1))
	if err != nil || len(raw) > oauthPendingMaxBytes {
		return nil, errOAuthStorage
	}
	var signed oauthSignedRecord
	if strictOAuthJSON(raw, &signed) != nil {
		return nil, errOAuthStorage
	}
	mac, err := hex.DecodeString(signed.MAC)
	h := hmac.New(sha256.New, s.key)
	h.Write(signed.Record)
	if err != nil || !hmac.Equal(mac, h.Sum(nil)) {
		return nil, errOAuthStorage
	}
	var rec oauthPendingRecord
	if strictOAuthJSON(signed.Record, &rec) != nil || !validOAuthPendingRecord(&rec) || oauthPendingName(rec.State) != name {
		return nil, errOAuthStorage
	}
	return &rec, nil
}

// Bounded work even in a hostile/full directory. Invalid records fail closed;
// janitor only deletes authenticated expired records and abandoned temp files.
func (s *oauthPendingStore) scan(now time.Time) (int, error) {
	fd, err := unix.Openat(s.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, errOAuthStorage
	}
	f := os.NewFile(uintptr(fd), "oauth-pending-directory")
	defer f.Close()
	entries, err := f.ReadDir(oauthPendingMaxFiles*2 + 3)
	if err != nil && err != io.EOF {
		return 0, errOAuthStorage
	}
	if len(entries) > oauthPendingMaxFiles*2+2 {
		return 0, errOAuthStorage
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if name == ".lock" || name == ".key" {
			continue
		}
		if strings.HasPrefix(name, ".tmp-") && !entry.IsDir() {
			if unix.Unlinkat(s.fd, name, 0) != nil {
				return 0, errOAuthStorage
			}
			continue
		}
		if validOAuthPendingName(name) {
			rec, err := s.read(name)
			if err != nil {
				return 0, errOAuthStorage
			}
			if !now.Before(rec.Expires) {
				if unix.Unlinkat(s.fd, name, 0) != nil {
					return 0, errOAuthStorage
				}
				continue
			}
		}
		count++
	}
	return count, nil
}

func (s *oauthPendingStore) write(rec *oauthPendingRecord) error {
	if !validOAuthPendingRecord(rec) {
		return errOAuthStorage
	}
	record, err := json.Marshal(rec)
	if err != nil {
		return errOAuthStorage
	}
	h := hmac.New(sha256.New, s.key)
	h.Write(record)
	raw, err := json.Marshal(oauthSignedRecord{Record: record, MAC: hex.EncodeToString(h.Sum(nil))})
	if err != nil || len(raw) > oauthPendingMaxBytes {
		return errOAuthStorage
	}
	count, err := s.scan(time.Now())
	if err != nil {
		return errOAuthStorage
	}
	name := oauthPendingName(rec.State)
	_, err = s.read(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errOAuthStorage
	}
	if err != nil && count >= oauthPendingMaxFiles {
		return errors.New("too many pending logins; wait for expiry")
	}
	return s.atomicWrite(name, raw)
}

func (s *oauthPendingStore) atomicWrite(name string, raw []byte) error {
	suffix, err := oauthRandomHex(16)
	if err != nil {
		return errOAuthStorage
	}
	temp := ".tmp-" + suffix
	fd, err := unix.Openat(s.fd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errOAuthStorage
	}
	f := os.NewFile(uintptr(fd), "oauth-pending-temp")
	defer func() { _ = f.Close(); _ = unix.Unlinkat(s.fd, temp, 0) }()
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		return errOAuthStorage
	}
	if f.Sync() != nil || f.Close() != nil {
		return errOAuthStorage
	}
	if unix.Renameat(s.fd, temp, s.fd, name) != nil || unix.Fsync(s.fd) != nil {
		return errOAuthStorage
	}
	return nil
}

func (s *oauthPendingStore) remove(name string) {
	if validOAuthPendingName(name) {
		_ = unix.Unlinkat(s.fd, name, 0)
		_ = unix.Fsync(s.fd)
	}
}
