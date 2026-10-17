// Package plantcx is a planted defect for the complexity gate proof; the
// pull request that adds it is closed unmerged.
package plantcx

// Depth nests six conditions, which exceeds cognitive 15 while cyclomatic
// stays at 7.
func Depth(a, b, c, d, e, f int) int {
	if a > 0 {
		if b > 0 {
			if c > 0 {
				if d > 0 {
					if e > 0 {
						if f > 0 {
							return 1
						}
					}
				}
			}
		}
	}
	return 0
}
