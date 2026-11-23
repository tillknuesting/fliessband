package metrics

// Acceptance reports the executable examples from the spec.
type Acceptance struct {
	Passed   int       `json:"passed"`
	Failed   int       `json:"failed"`
	Pending  int       `json:"pending"` // methods still stubbed
	Holdout  int       `json:"holdout"` // examples hidden from the LLM
	Examples []Example `json:"examples"`
}

// Example is the result of one acceptance example.
type Example struct {
	Method  string `json:"method"`
	Name    string `json:"name"`
	Holdout bool   `json:"holdout"`
	Status  string `json:"status"` // passed, failed, pending
	Message string `json:"message,omitempty"`
}

// Mutation reports mutation testing: small faults injected into the code,
// counted as killed when a test fails. It shows whether tests check
// behaviour, which statement coverage cannot.
type Mutation struct {
	Total      int            `json:"total"`
	Killed     int            `json:"killed"`
	Survived   int            `json:"survived"`
	TimedOut   int            `json:"timed_out"`          // counts as killed
	NotCovered int            `json:"not_covered"`        // no test runs the line
	Invalid    int            `json:"invalid"`            // did not compile
	Equivalent int            `json:"equivalent"`         // cannot be killed, judged by the LLM
	Skipped    int            `json:"skipped"`            // over the time or count budget
	Score      *float64       `json:"score,omitempty"`    // detected / (detected + survived + not covered)
	Strength   *float64       `json:"strength,omitempty"` // detected / (detected + survived)
	Sampled    bool           `json:"sampled"`
	DurationMs float64        `json:"duration_ms"`
	ByOperator []OperatorStat `json:"by_operator"`
	ByFile     []FileMutation `json:"by_file"`
	Survivors  []Mutant       `json:"survivors"`
}

// OperatorStat counts mutants of one operator.
type OperatorStat struct {
	Operator string `json:"operator"`
	Total    int    `json:"total"`
	Killed   int    `json:"killed"`
}

// FileMutation counts mutants of one file.
type FileMutation struct {
	File   string   `json:"file"`
	Origin string   `json:"origin"`
	Total  int      `json:"total"`
	Killed int      `json:"killed"`
	Score  *float64 `json:"score,omitempty"`
}

// Mutant is one surviving (or otherwise notable) mutant.
type Mutant struct {
	ID       int    `json:"id"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Operator string `json:"operator"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
	Before   string `json:"before"` // the source line
	After    string `json:"after"`  // the line with the mutation applied
	Origin   string `json:"origin"`
	Status   string `json:"status"`
}

// Fuzz reports the fuzzing stage.
type Fuzz struct {
	Targets    []FuzzTarget `json:"targets"`
	Execs      int64        `json:"execs"`
	Failures   int          `json:"failures"`
	DurationMs float64      `json:"duration_ms"`
}

// FuzzTarget is one fuzz function.
type FuzzTarget struct {
	Name        string `json:"name"`
	Execs       int64  `json:"execs"`
	Interesting int    `json:"interesting"` // new inputs that reached new code
	Failed      bool   `json:"failed"`
	Input       string `json:"input,omitempty"` // failing input file
}

// Contract reports the compatibility check against the last passing run.
type Contract struct {
	Baseline string    `json:"baseline,omitempty"` // run id
	Package  string    `json:"package"`
	Changes  []Finding `json:"changes"`
	Note     string    `json:"note,omitempty"`
}

// Integration reports the tests against real dependencies.
type Integration struct {
	Database string  `json:"database"`
	Tests    int     `json:"tests"`
	Failed   int     `json:"failed"`
	StartMs  float64 `json:"start_ms"`
}

// Supply reports the supply-chain artifacts of the build.
type Supply struct {
	Components   int         `json:"components"`
	GoVersion    string      `json:"go_version"`
	BinarySHA256 string      `json:"binary_sha256"`
	Modules      []Module    `json:"modules"`
	Settings     [][2]string `json:"settings"`
	SBOMFile     string      `json:"sbom_file"`
	Provenance   string      `json:"provenance_file"`
}

// Module is one dependency linked into the binary.
type Module struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Sum     string `json:"sum,omitempty"`
}
