package main

// nearCommand avoids unrelated suggestions: the CLI library returns its best
// match even when two commands share only a single letter.
func nearCommand(a, b string) bool {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := range len(a) {
		next := make([]int, len(b)+1)
		next[0] = i + 1
		for j := range len(b) {
			cost := 0
			if a[i] != b[j] {
				cost = 1
			}
			next[j+1] = min(previous[j+1]+1, next[j]+1, previous[j]+cost)
		}
		previous = next
	}
	return previous[len(b)] <= 2
}
