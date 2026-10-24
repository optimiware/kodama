package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

const maxCredentialFileBytes = 64 << 10

// DefaultFileStoreDir is <user config dir>/kodama/credentials.
func DefaultFileStoreDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the user configuration directory: %w", err)
	}
	return filepath.Join(dir, "kodama", "credentials"), nil
}

// FileStore keeps one 0600 file per credential in a protected directory. It is
// selected only by an explicit `--credential-store file`; it is never a
// fallback. Directories must be real directories with no group/other access;
// credential files must be regular files with mode exactly 0600. A symlink,
// device, or loosened permission is rejected, never silently repaired. Writes
// go to a new 0600 file that is flushed and renamed over the target.
type FileStore struct {
	dir    string
	rename func(oldpath, newpath string) error // replaceable in tests
}

var _ CredentialStore = (*FileStore)(nil)

// NewFileStore stores credentials under dir.
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir, rename: os.Rename} }

func (s *FileStore) path(key string) string { return filepath.Join(s.dir, key+".json") }

func (s *FileStore) check() error {
	if runtime.GOOS == "windows" {
		return storeErr(ErrInsecureStore, nil, "the file credential store needs POSIX permissions and is unsupported on Windows; use the keyring")
	}
	return nil
}

// ensureDir creates the directory with 0700 when absent and verifies it.
func (s *FileStore) ensureDir(create bool) error {
	fi, err := os.Lstat(s.dir)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return storeErr(ErrPersistenceFailed, err, "cannot create the credential directory")
		}
		fi, err = os.Lstat(s.dir)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return storeErr(ErrInsecureStore, nil, "%s is not a plain directory", s.dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return storeErr(ErrInsecureStore, nil, "directory %s is accessible by other users (mode %o); chmod 700 it", s.dir, fi.Mode().Perm())
	}
	return nil
}

func (s *FileStore) Load(ctx context.Context, key string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, ctxFailure(err)
	}
	if err := validateKey(key); err != nil {
		return Credential{}, err
	}
	if err := s.check(); err != nil {
		return Credential{}, err
	}
	if err := s.ensureDir(false); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credential{}, storeErr(ErrCredentialNotFound, err, "no credential file")
		}
		return Credential{}, wrapDirErr(err)
	}
	p := s.path(key)
	li, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Credential{}, storeErr(ErrCredentialNotFound, err, "no credential file")
	}
	if err != nil {
		return Credential{}, storeErr(ErrStoreUnavailable, err, "the credential file cannot be inspected")
	}
	if err := checkFile(p, li); err != nil {
		return Credential{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return Credential{}, storeErr(ErrStoreUnavailable, err, "the credential file cannot be opened")
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !os.SameFile(li, fi) {
		return Credential{}, storeErr(ErrInsecureStore, err, "the credential file changed while it was opened")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCredentialFileBytes+1))
	if err != nil || len(b) > maxCredentialFileBytes {
		return Credential{}, storeErr(ErrStoreUnavailable, err, "the credential file cannot be read")
	}
	c, err := decodeCredential(b)
	if err != nil {
		return Credential{}, storeErr(ErrStoreUnavailable, err, "the credential file is unreadable; run `kodama auth login` again")
	}
	return c, nil
}

func checkFile(p string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return storeErr(ErrInsecureStore, nil, "%s is not a regular file (symlinks are refused)", p)
	}
	if fi.Mode().Perm() != 0o600 {
		return storeErr(ErrInsecureStore, nil, "%s has mode %o, want 600; fix it with chmod 600 or delete it", p, fi.Mode().Perm())
	}
	return nil
}

func wrapDirErr(err error) error {
	var se *StoreError
	if errors.As(err, &se) {
		return err
	}
	return storeErr(ErrStoreUnavailable, err, "the credential directory cannot be inspected")
}

func (s *FileStore) Save(ctx context.Context, key string, c Credential) (err error) {
	if err := ctx.Err(); err != nil { // cancelled: nothing is created or replaced
		return ctxFailure(err)
	}
	if err := validateKey(key); err != nil {
		return err
	}
	if err := s.check(); err != nil {
		return err
	}
	data, err := encodeCredential(c)
	if err != nil {
		return storeErr(ErrPersistenceFailed, err, "the credential cannot be encoded")
	}
	if err := s.ensureDir(true); err != nil {
		return wrapDirErr(err)
	}
	p := s.path(key)
	if li, lerr := os.Lstat(p); lerr == nil {
		if err := checkFile(p, li); err != nil { // never replace or "repair" an insecure destination
			return err
		}
	} else if !errors.Is(lerr, fs.ErrNotExist) {
		return storeErr(ErrPersistenceFailed, lerr, "the destination cannot be inspected")
	}
	tmp, err := os.CreateTemp(s.dir, ".cred-*.tmp")
	if err != nil {
		return storeErr(ErrPersistenceFailed, err, "cannot create a temporary credential file")
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// Last point of no return: after the rename the credential is
		// committed, so cancellation is honoured only before it.
		if cerr := ctx.Err(); cerr != nil {
			return ctxFailure(cerr) // the deferred cleanup removes the temporary file
		}
		err = s.rename(name, p)
	}
	if err != nil {
		return storeErr(ErrPersistenceFailed, err, "the credential file could not be written")
	}
	if d, derr := os.Open(s.dir); derr == nil { // best-effort directory flush
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Delete removes the credential file. A missing file or directory is success.
// Removal never follows a symlink and cannot disclose anything, so it does not
// require the permission checks that reading does.
func (s *FileStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil { // cancelled: the credential stays in place
		return ctxFailure(err)
	}
	if err := validateKey(key); err != nil {
		return err
	}
	if err := s.check(); err != nil {
		return err
	}
	err := os.Remove(s.path(key))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return storeErr(ErrPersistenceFailed, err, "the credential file could not be removed")
}
