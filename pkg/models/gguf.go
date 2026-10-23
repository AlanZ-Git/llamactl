package models

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

var errGGUFUnsupported = errors.New("unsupported GGUF metadata")

type ggufMetadata struct {
	Architecture    string   `json:"architecture"`
	Name            string   `json:"name,omitempty"`
	SplitCount      uint64   `json:"split_count,omitempty"`
	SplitNo         uint64   `json:"split_no,omitempty"`
	SplitTensors    uint64   `json:"split_tensors,omitempty"`
	Tensors         uint64   `json:"tensors"`
	EmbeddingLength uint64   `json:"embedding_length,omitempty"`
	ProjectionDim   uint64   `json:"projection_dim,omitempty"`
	TensorNames     []string `json:"-"`
}
type ggufReader struct{ r *io.LimitedReader }

func (g *ggufReader) read(v any) error { return binary.Read(g.r, binary.LittleEndian, v) }
func (g *ggufReader) str() (string, error) {
	var n uint64
	if err := g.read(&n); err != nil {
		return "", err
	}
	if n > 1<<20 || n > uint64(g.r.N) {
		return "", fmt.Errorf("metadata string limit")
	}
	b := make([]byte, int(n))
	_, err := io.ReadFull(g.r, b)
	return string(b), err
}
func (g *ggufReader) value(typ uint32, depth int) (any, error) {
	switch typ {
	case 0:
		var v uint8
		err := g.read(&v)
		return uint64(v), err
	case 1:
		var v int8
		err := g.read(&v)
		return int64(v), err
	case 2:
		var v uint16
		err := g.read(&v)
		return uint64(v), err
	case 3:
		var v int16
		err := g.read(&v)
		return int64(v), err
	case 4:
		var v uint32
		err := g.read(&v)
		return uint64(v), err
	case 5:
		var v int32
		err := g.read(&v)
		return int64(v), err
	case 6:
		var v float32
		err := g.read(&v)
		return v, err
	case 7:
		var v uint8
		err := g.read(&v)
		if v > 1 {
			return nil, fmt.Errorf("invalid bool")
		}
		return v == 1, err
	case 8:
		return g.str()
	case 9:
		if depth != 0 {
			return nil, errGGUFUnsupported
		}
		var t uint32
		var n uint64
		if err := g.read(&t); err != nil {
			return nil, err
		}
		if err := g.read(&n); err != nil {
			return nil, err
		}
		if n > 1<<20 {
			return nil, fmt.Errorf("metadata array limit")
		}
		for j := uint64(0); j < n; j++ {
			if _, err := g.value(t, depth+1); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case 10:
		var v uint64
		err := g.read(&v)
		return v, err
	case 11:
		var v int64
		err := g.read(&v)
		return v, err
	case 12:
		var v float64
		err := g.read(&v)
		return v, err
	default:
		return nil, errGGUFUnsupported
	}
}
func parseGGUF(f *os.File) (ggufMetadata, error) {
	var meta ggufMetadata
	var err error
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return meta, err
	}
	g := ggufReader{&io.LimitedReader{R: f, N: 16 << 20}}
	var magic [4]byte
	var version uint32
	var kv uint64
	if err = g.read(&magic); err != nil {
		return meta, err
	}
	if string(magic[:]) != "GGUF" {
		return meta, fmt.Errorf("not GGUF")
	}
	if err = g.read(&version); err != nil {
		return meta, err
	}
	if version != 2 && version != 3 {
		return meta, errGGUFUnsupported
	}
	if err = g.read(&meta.Tensors); err != nil {
		return meta, err
	}
	if err = g.read(&kv); err != nil {
		return meta, err
	}
	if kv > 65536 || meta.Tensors == 0 || meta.Tensors > 100000 {
		return meta, fmt.Errorf("GGUF count limit")
	}
	seen := map[string]bool{}
	embeddings := map[string]uint64{}
	for j := uint64(0); j < kv; j++ {
		key, e := g.str()
		if e != nil {
			return meta, e
		}
		if seen[key] {
			return meta, fmt.Errorf("duplicate metadata")
		}
		seen[key] = true
		var typ uint32
		if e = g.read(&typ); e != nil {
			return meta, e
		}
		v, e := g.value(typ, 0)
		if e != nil {
			return meta, e
		}
		if key == "clip.vision.projection_dim" || strings.HasSuffix(key, ".embedding_length") {
			n, ok := v.(uint64)
			if !ok || n == 0 {
				return meta, fmt.Errorf("embedding dimension type or size")
			}
			if key == "clip.vision.projection_dim" {
				meta.ProjectionDim = n
			} else {
				embeddings[key] = n
			}
		}
		switch key {
		case "general.architecture":
			var ok bool
			meta.Architecture, ok = v.(string)
			if !ok {
				return meta, fmt.Errorf("architecture type")
			}
		case "general.name":
			meta.Name, _ = v.(string)
		case "split.count":
			var ok bool
			meta.SplitCount, ok = v.(uint64)
			if !ok || meta.SplitCount < 2 || meta.SplitCount > 1024 {
				return meta, fmt.Errorf("split count")
			}
		case "split.no":
			var ok bool
			meta.SplitNo, ok = v.(uint64)
			if !ok {
				return meta, fmt.Errorf("split index")
			}
		case "split.tensors.count":
			switch n := v.(type) {
			case uint64:
				meta.SplitTensors = n
			case int64:
				if n > 0 {
					meta.SplitTensors = uint64(n)
				}
			}
		}
	}
	meta.EmbeddingLength = embeddings[meta.Architecture+".embedding_length"]
	if meta.Architecture == "" {
		return meta, errGGUFUnsupported
	}
	if meta.SplitCount > 0 && (!seen["split.no"] || meta.SplitNo >= meta.SplitCount || meta.SplitTensors < meta.Tensors) {
		return meta, fmt.Errorf("split metadata")
	}
	// Parse bounded descriptors for identity and split consistency only.
	// Tensor types, payload geometry/alignment and weight integrity belong to
	// the engine loader; an identity-verified file is not necessarily runnable.
	names := map[string]bool{}
	for j := uint64(0); j < meta.Tensors; j++ {
		name, e := g.str()
		if e != nil {
			return meta, e
		}
		if names[name] || name == "" {
			return meta, fmt.Errorf("tensor name")
		}
		names[name] = true
		meta.TensorNames = append(meta.TensorNames, name)
		var dims, typ uint32
		var offset uint64
		if e = g.read(&dims); e != nil {
			return meta, e
		}
		if dims < 1 || dims > 4 {
			return meta, fmt.Errorf("tensor dimensions")
		}
		count := uint64(1)
		for d := uint32(0); d < dims; d++ {
			var size uint64
			if e = g.read(&size); e != nil {
				return meta, e
			}
			if size == 0 || size > 1<<40 || count > (1<<50)/size {
				return meta, fmt.Errorf("tensor size")
			}
			count *= size
		}
		if e = g.read(&typ); e != nil {
			return meta, e
		}
		if e = g.read(&offset); e != nil {
			return meta, e
		}
	}
	return meta, nil
}
