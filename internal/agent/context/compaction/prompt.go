package compaction

// summaryContentRules is what any summary request asks the summary to keep.
const summaryContentRules = `Decide from the content itself what still matters and how long the summary needs to be. Condense repetition, routine output, and superseded detail heavily. Keep in full what later steps depend on:
- the user's goals, requests, preferences, and constraints that still apply
- facts and data that were given to be kept or that later steps rely on
- decisions made and their reasons
- the state of the work: what was planned, what is done, what is still open or owed, and what was attempted with an unknown result
- errors and failed attempts that are still relevant, so they are not repeated
- exact names, identifiers, paths, commands, URLs, numbers, and dates
- the ref=<id> of each tool result later steps may need to read again in full; get_messages reads it back by that ref`
