package manager

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// WriteDeploymentCSV exports the same sanitized report as JSON in lossless
// field/value rows. Spreadsheet formula prefixes are quoted as literal text.
func WriteDeploymentCSV(w io.Writer, report DeploymentTest) error {
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	csvWriter := csv.NewWriter(w)
	safe := func(s string) string {
		if len(s) > 0 && strings.ContainsAny(s[:1], "=+-@\t\r\n") {
			return "'" + s
		}
		return s
	}
	if err := csvWriter.Write([]string{"field", "value"}); err != nil {
		return err
	}
	var visit func(string, any) error
	visit = func(path string, v any) error {
		switch x := v.(type) {
		case map[string]any:
			if len(x) == 0 {
				return csvWriter.Write([]string{safe(path), "{}"})
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				if err := visit(p, x[k]); err != nil {
					return err
				}
			}
		case []any:
			if len(x) == 0 {
				return csvWriter.Write([]string{safe(path), "[]"})
			}
			for n, item := range x {
				if err := visit(path+"["+strconv.Itoa(n)+"]", item); err != nil {
					return err
				}
			}
		default:
			s := "null"
			if x != nil {
				s = fmt.Sprint(x)
			}
			return csvWriter.Write([]string{safe(path), safe(s)})
		}
		return nil
	}
	if err := visit("", value); err != nil {
		return err
	}
	csvWriter.Flush()
	return csvWriter.Error()
}
