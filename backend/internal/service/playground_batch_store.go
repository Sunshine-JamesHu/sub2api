package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"io"
	"os"
	"path/filepath"
)

const batchStoreAAD = "sub2api-playground-batch-v1"

type playgroundBatchStore struct {
	directory string
	aead      cipher.AEAD
	lock      *os.File
}

func openPlaygroundBatchStore(directory, secret string) (*playgroundBatchStore, error) {
	key, err := hex.DecodeString(secret)
	if err != nil || len(key) != 32 {
		return nil, errors.New("批量上号需要固定的 TOTP 加密密钥")
	}
	derive := hmac.New(sha256.New, key)
	_, _ = derive.Write([]byte(batchStoreAAD))
	block, err := aes.NewCipher(derive.Sum(nil))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(directory, 0700); err != nil {
		return nil, err
	}
	lock, err := lockPlaygroundBatchStore(filepath.Join(directory, "owner.lock"))
	if err != nil {
		return nil, err
	}
	return &playgroundBatchStore{directory: directory, aead: aead, lock: lock}, nil
}

func (s *playgroundBatchStore) Load() (*playgroundBatchState, error) {
	raw, err := os.ReadFile(filepath.Join(s.directory, "state.enc"))
	if errors.Is(err, os.ErrNotExist) {
		return &playgroundBatchState{Version: 1, Concurrency: 2, Batches: []*playgroundBatch{}}, nil
	}
	if err != nil {
		return nil, err
	}
	n := s.aead.NonceSize()
	if len(raw) < n+s.aead.Overhead() || len(raw) > 64<<20 {
		return nil, errors.New("任务文件损坏或过大")
	}
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(batchStoreAAD))
	if err != nil {
		return nil, errors.New("无法解密上号任务，请检查持久化加密密钥")
	}
	defer clear(plain)
	var state playgroundBatchState
	if json.Unmarshal(plain, &state) != nil || state.Version != 1 {
		return nil, errors.New("上号任务格式不正确")
	}
	return &state, nil
}

func (s *playgroundBatchStore) Save(state *playgroundBatchState) error {
	plain, err := json.Marshal(state)
	if err != nil {
		return err
	}
	defer clear(plain)
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	raw := s.aead.Seal(nonce, nonce, plain, []byte(batchStoreAAD))
	f, err := os.CreateTemp(s.directory, ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(s.directory, "state.enc")); err != nil {
		return err
	}
	if d, e := os.Open(s.directory); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *playgroundBatchStore) Close() {
	if s.lock != nil {
		_ = s.lock.Close()
	}
}

func (s *playgroundBatchStore) framePath(id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", errors.New("invalid task identity")
	}
	return filepath.Join(s.directory, "frames", id+".enc"), nil
}
func (s *playgroundBatchStore) SaveFrame(id, frame string) error {
	if len(frame) > 4<<20 {
		return errors.New("frame too large")
	}
	path, err := s.framePath(id)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	raw := s.aead.Seal(nonce, nonce, []byte(frame), []byte(batchStoreAAD+":frame:"+id))
	f, err := os.CreateTemp(filepath.Dir(path), ".frame-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func (s *playgroundBatchStore) LoadFrame(id string) (string, error) {
	path, err := s.framePath(id)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("暂无截图，请打开人工窗口")
	}
	n := s.aead.NonceSize()
	if len(raw) < n+s.aead.Overhead() || len(raw) > 5<<20 {
		return "", errors.New("截图不可用")
	}
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(batchStoreAAD+":frame:"+id))
	if err != nil {
		return "", errors.New("截图不可用")
	}
	defer clear(plain)
	return string(plain), nil
}
func (s *playgroundBatchStore) RemoveFrame(id string) {
	if path, err := s.framePath(id); err == nil {
		_ = os.Remove(path)
	}
}
