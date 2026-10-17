package scripts_test

// Helpers shared by the fork's two embedded-gate pins,
// fork_embedded_gated_lane_pin_test.go (scripts_test) and
// fork_embedded_gate_scan_pin_test.go (go_sources_test), kept apart so that
// each go_test in scripts/BUILD.bazel compiles only the pin whose tests it
// runs (and so declares only the data that pin reads). Helpers only: both
// targets compile this file, so a test here would run in both.

import "sort"

const (
	forkEmbeddedGatedWorkflow = "fork-embedded-gated.yml"
	embeddedDoltGate          = "BEADS_TEST_EMBEDDED_DOLT"
)

// forkDoctorGatedTests: the gated tests of cmd/bd/doctor. The workflow's -run
// pattern, its GATED_TESTS and the guard are all compared against this one
// list.
var forkDoctorGatedTests = []string{
	"TestCheckMigrationContentSkew_EmbeddedFallback",
	"TestCheckVersionableIssues_EmbeddedStoreAgreesWithTheMint",
}

// listDiff: what to add to, and what to drop from, have to make it equal want
// (both sorted, no duplicates).
func listDiff(have, want []string) (add, drop []string) {
	in := func(list []string, name string) bool {
		i := sort.SearchStrings(list, name)
		return i < len(list) && list[i] == name
	}
	for _, name := range want {
		if !in(have, name) {
			add = append(add, name)
		}
	}
	for _, name := range have {
		if !in(want, name) {
			drop = append(drop, name)
		}
	}
	return add, drop
}
