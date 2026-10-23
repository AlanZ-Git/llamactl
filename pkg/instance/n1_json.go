package instance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// StrictJSON rejects duplicate fields at every depth before typed decoding.
func StrictJSON(data []byte, out any) error {
	if len(data) > 1<<20 {
		return fmt.Errorf("JSON limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int, string) error
	walk = func(depth int, key string) error {
		if depth > 32 {
			return fmt.Errorf("JSON depth")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		if t == nil {
			switch key {
			case "evidence", "capacity_interval", "supersedes", "context", "gpu_layers", "threads", "batch", "ubatch", "cache_ram", "kv_type_k", "kv_type_v":
				return nil
			default:
				return fmt.Errorf("null is not allowed for %s", key)
			}
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return fmt.Errorf("duplicate JSON field")
				}
				seen[s] = true
				if e = walk(depth+1, s); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(depth+1, ""); e != nil {
					return e
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0, ""); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	return d.Decode(out)
}
