package search

import "strings"

// districtAliasGroups lists districts that should be treated as
// interchangeable for the `district` filter, even though they are stored as
// distinct rows in `locations`. This is NOT fuzzy/similarity matching — it's
// an explicit, curated list of known real-world naming overlaps (e.g. "New
// Delhi" is technically one district within the wider Delhi NCT, but most
// listings and searches use "Delhi" generically). Add an entry here only for
// a confirmed case, not speculatively for every city.
var districtAliasGroups = [][]string{
	{"delhi", "new delhi"},
}

var districtAliasIndex = buildDistrictAliasIndex(districtAliasGroups)

func buildDistrictAliasIndex(groups [][]string) map[string][]string {
	idx := make(map[string][]string)
	for _, group := range groups {
		for _, member := range group {
			idx[member] = group
		}
	}
	return idx
}

// expandDistrictAliases returns the lowercased set of district names that
// should match a search for `district` — itself, plus any known aliases.
func expandDistrictAliases(district string) []string {
	key := strings.ToLower(strings.TrimSpace(district))
	if group, ok := districtAliasIndex[key]; ok {
		return group
	}
	return []string{key}
}
