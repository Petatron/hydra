package cluster

import (
	"github.com/pmezard/go-difflib/difflib"
)

// unifiedDiff returns a unified diff of two renderings of one object.
func unifiedDiff(before, after string) string {
	d, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(before),
		B:        difflib.SplitLines(after),
		FromFile: "live",
		ToFile:   "rendered",
		Context:  2,
	})
	if err != nil {
		return err.Error()
	}
	return d
}
