package dmsgpty

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/SkycoinProject/dmsg/cipher"
)

// Whitelist represents a whitelist of public keys.
type Whitelist interface {
	Get(pk cipher.PubKey) (bool, error)
	All() (map[cipher.PubKey]bool, error)
	Add(pks ...cipher.PubKey) error
	Remove(pks ...cipher.PubKey) error
}

// NewJSONFileWhiteList creates a JSON file implementation of a whitelist.
func NewJSONFileWhiteList(fileName string) (Whitelist, error) {
	fileName, err := filepath.Abs(fileName)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(fileName), 0750); err != nil {
		return nil, err
	}

	return &jsonFileWhitelist{fileName: fileName}, nil
}

type jsonFileWhitelist struct {
	fileName string
}

func (w *jsonFileWhitelist) Get(pk cipher.PubKey) (bool, error) {
	var ok bool
	err := w.open(os.O_RDONLY|os.O_CREATE, func(pkMap map[cipher.PubKey]bool, _ *os.File) error {
		ok = pkMap[pk]
		return nil
	})
	return ok, jsonFileErr(err)
}

func (w *jsonFileWhitelist) All() (map[cipher.PubKey]bool, error) {
	var out map[cipher.PubKey]bool
	err := w.open(os.O_RDONLY|os.O_CREATE, func(pkMap map[cipher.PubKey]bool, _ *os.File) error {
		out = pkMap
		return nil
	})
	return out, jsonFileErr(err)
}

func (w *jsonFileWhitelist) Add(pks ...cipher.PubKey) error {
	return jsonFileErr(w.open(os.O_RDWR|os.O_CREATE, func(pkMap map[cipher.PubKey]bool, f *os.File) error {
		for _, pk := range pks {
			pkMap[pk] = true
		}
		return json.NewEncoder(f).Encode(pkMap)
	}))
}

func (w *jsonFileWhitelist) Remove(pks ...cipher.PubKey) error {
	return jsonFileErr(w.open(os.O_RDWR|os.O_CREATE, func(pkMap map[cipher.PubKey]bool, f *os.File) error {
		for _, pk := range pks {
			delete(pkMap, pk)
		}
		return json.NewEncoder(f).Encode(pkMap)
	}))
}

func (w *jsonFileWhitelist) open(perm int, fn func(pkMap map[cipher.PubKey]bool, f *os.File) error) error {
	f, err := os.OpenFile(w.fileName, perm, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck

	// get file size
	info, err := f.Stat()
	if err != nil {
		return err
	}

	// read public key map from file
	pks := make(map[cipher.PubKey]bool)
	if info.Size() > 0 {
		if err := json.NewDecoder(f).Decode(&pks); err != nil {
			return err
		}
	}

	// seek back to start of file
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}

	return fn(pks, f)
}

func jsonFileErr(err error) error {
	if err != nil {
		return fmt.Errorf("json file whitelist: %v", err)
	}
	return nil
}

// NewMemoryWhitelist creates a memory implementation of a whitelist.
func NewMemoryWhitelist() Whitelist {
	return &memoryWhitelist{
		m: make(map[cipher.PubKey]struct{}),
	}
}

type memoryWhitelist struct {
	m   map[cipher.PubKey]struct{}
	mux sync.RWMutex
}

func (w *memoryWhitelist) Get(pk cipher.PubKey) (bool, error) {
	w.mux.RLock()
	_, ok := w.m[pk]
	w.mux.RUnlock()
	return ok, nil
}

func (w *memoryWhitelist) All() (map[cipher.PubKey]bool, error) {
	out := make(map[cipher.PubKey]bool)
	w.mux.RLock()
	for k := range w.m {
		out[k] = true
	}
	w.mux.RUnlock()
	return out, nil
}

func (w *memoryWhitelist) Add(pks ...cipher.PubKey) error {
	w.mux.Lock()
	for _, pk := range pks {
		w.m[pk] = struct{}{}
	}
	w.mux.Unlock()
	return nil
}

func (w *memoryWhitelist) Remove(pks ...cipher.PubKey) error {
	w.mux.Lock()
	for _, pk := range pks {
		delete(w.m, pk)
	}
	w.mux.Unlock()
	return nil
}