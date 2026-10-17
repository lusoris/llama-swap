// Package plantcx is a planted defect for the complexity gate proof; the
// pull request that adds it is closed unmerged.
package plantcx

// Grade nests enough branches to exceed cyclomatic 10 and cognitive 15.
func Grade(a, b, c int) int {
	if a > 0 {
		if b > 0 {
			if c > 0 {
				return 1
			} else if c < -10 {
				return 2
			}
		} else if b < -10 {
			if c > 10 {
				return 3
			}
		}
	} else if a < -10 {
		if b > 10 && c > 10 {
			return 4
		} else if b < 0 || c < 0 {
			return 5
		}
	}
	if a == b || b == c {
		return 6
	}
	return 0
}
