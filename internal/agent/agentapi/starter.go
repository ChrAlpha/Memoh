package agentapi

// StarterPrompt is one suggested message for the new-chat screen.
type StarterPrompt struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
}

// StarterPromptsRequest identifies whose suggestions to produce. KnownHash is
// the hash of the set the client already holds, empty when it holds none.
// Refresh asks for a different set even when the held one is current; Avoid
// lists the prompts already on screen, which the new set must not repeat.
type StarterPromptsRequest struct {
	BotID     string
	UserID    string
	Locale    string
	KnownHash string
	Refresh   bool
	Avoid     []string
}

// StarterPromptsResult is either Unchanged (the client's set still matches
// the material) or a fresh set with its Hash. A set the model could not
// produce comes back with an empty Hash and no Items; the client shows its
// generic examples and asks again next time.
type StarterPromptsResult struct {
	Unchanged bool
	Hash      string
	Items     []StarterPrompt
}
