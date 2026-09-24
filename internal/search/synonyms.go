package search

import "strings"

// districtAliasGroups lists districts that should be treated as
// interchangeable for the `district` filter, even though they are stored as
// distinct rows in `locations`. This is NOT fuzzy/similarity matching — it's
// an explicit, curated list of known real-world naming overlaps. Each entry
// was confirmed against the live `locations` data (both spellings actually
// exist as distinct district values there) before being added — this list
// is not a guess at "every Indian city that might have an alias."
//
// Deliberately NOT aliased, despite looking similar: "Mumbai"/"Navi Mumbai"
// and "Noida"/"Greater Noida" are genuinely distinct places (different
// municipal areas), not renames of the same place, so merging them would be
// wrong rather than helpful.
var districtAliasGroups = [][]string{
	{"delhi", "new delhi"},
	{"bangalore", "bengaluru"}, // official rename, 2014; both still in common use
	{"gurgaon", "gurugram"},    // official rename, 2016; both still in common use
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
