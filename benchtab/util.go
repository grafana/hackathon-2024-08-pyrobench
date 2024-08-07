// Heavily adapted from https://cs.opensource.google/go/x/perf/+/master:cmd/benchstat/main.go;l=446-490.

package benchtab

import (
	"fmt"

	"golang.org/x/perf/benchproc"
)

func NewDefaultBuilder() (*Builder, *benchproc.Filter, error) {
	const defaultTable = ".config"
	const defaultRow = "name"
	const defaultCol = "source"
	const defaultFilter = "*"

	filter, err := benchproc.NewFilter(defaultFilter)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing -filter: %s", err)
	}

	var parser benchproc.ProjectionParser
	var parseErr error
	mustParse := func(name, val string, unit bool) *benchproc.Projection {
		var proj *benchproc.Projection
		var err error
		if unit {
			proj, _, err = parser.ParseWithUnit(val, filter)
		} else {
			proj, err = parser.Parse(val, filter)
		}
		if err != nil && parseErr == nil {
			parseErr = fmt.Errorf("parsing %s: %s", name, err)
		}
		return proj
	}

	tableBy := mustParse("-table", defaultTable, true)
	rowBy := mustParse("-row", defaultRow, false)
	colBy := mustParse("-col", defaultCol, false)
	residue := parser.Residue()
	if parseErr != nil {
		return nil, nil, parseErr
	}

	stat := NewBuilder(tableBy, rowBy, colBy, residue)
	return stat, filter, nil
}
