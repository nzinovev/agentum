package pack

// Overrides is the consumer override document. It selects a base pack (layers
// 1–2) and mutates it (layers 3–4). The schema:
//
//	base: java-spring@^1     # L1 lock major  |  fork: true  # L2 detach
//	prompts:                  # L3 swap prompt files
//	  implement: my-implement.md
//	stages:                   # L4 patch stage params
//	  implement:
//	    gate: human_approval
//	    tier: strong
//	budgets:                  # L4 patch budgets
//	  fix_cycles: 5
//
// Layer 1 (selecting the base by version constraint) is handled by Source — a
// Source resolves "base" to a loaded *Pack. Layers 2–4 are applied by Resolve.
type Overrides struct {
	Base    string                `yaml:"base"`    // "name", "name@^MAJOR", "name@X.Y.Z"
	Fork    bool                  `yaml:"fork"`    // L2: detach from upstream
	Prompts map[string]string     `yaml:"prompts"` // L3: stage id -> prompt file path (rel to override dir)
	Stages  map[string]StagePatch `yaml:"stages"`  // L4: stage id -> param patch
	Budgets *BudgetsPatch         `yaml:"budgets"` // L4: budget patch

	// promptText holds the loaded contents of each overridden prompt file,
	// keyed by stage id. Populated by LoadOverrides.
	promptText map[string]string
	dir        string // absolute dir the override document was loaded from
}

// StagePatch is a layer-4 patch on a single stage. Pointer fields distinguish
// "omit" from "set to zero value": a nil Gate means "do not change the gate."
type StagePatch struct {
	Gate *Gate   `yaml:"gate,omitempty"`
	Tier *string `yaml:"tier,omitempty"`
}

// BudgetsPatch patches the per-pack budgets. Same nil-means-omit convention.
type BudgetsPatch struct {
	FixCycles *int `yaml:"fix_cycles,omitempty"`
	AskToEdit *int `yaml:"ask_to_edit,omitempty"`
}
