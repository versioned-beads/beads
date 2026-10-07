package driver

// BaselineFinding is what comparing the seeded work clone with the oracle at the
// base came to, before the first step is replayed. It is counts only: a corpus's
// issues are never named in anything a run writes.
type BaselineFinding struct {
	// Compared is the number of issues the oracle held at the base.
	Compared int `json:"compared"`
	// Differing is how many of them the work clone does not hold exactly as the
	// oracle did. Zero says the seed copied the base without changing it.
	Differing int `json:"differing"`
}

// EnableRefusal records that the product refused to turn versioned history on:
// some issue holds a value a version could not record. The run goes on without
// versioning and says so. Only the number is kept, because the product's own
// report names the issues.
type EnableRefusal struct {
	// Count is the number of issues the product named as unversionable.
	Count int `json:"count"`
}
