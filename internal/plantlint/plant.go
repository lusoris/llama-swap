// Package plantlint is a planted defect for the golangci-lint gate proof;
// the pull request that adds it is closed unmerged.
package plantlint

// Double returns twice n; its first assignment is never read, which
// ineffassign reports.
func Double(n int) int {
	r := n
	r = n * 2
	return r
}
