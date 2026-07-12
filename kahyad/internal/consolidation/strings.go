package consolidation

// Turkish, user-facing strings (CLAUDE.md language policy) - byte-exact
// per the task spec, verified against tasks/w5-proactivity/
// W5-02-nightly-consolidation.md's own Context section (both contain an
// em-dash "—", not a hyphen).

// MsgLocalSkipped is the FAIL-CLOSED notice emitted whenever the local
// Qwen3-30B-A3B lane is unavailable (insufficient free memory, or the
// spawn/health gate itself failing) and secret-lane files are therefore
// skipped THIS RUN - NEVER routed to the cloud lane instead (HANDOFF §4 ⚑
// memory-pressure invariant).
const MsgLocalSkipped = "yerel model için bellek yok — gizli-şerit dosyaları bu gece atlandı"

// MsgSuggestionReady is the Telegram notice sent once a suggestion-mode
// consolidation run has produced a pending diff (task spec step 7).
const MsgSuggestionReady = "Konsolidasyon önerisi hazır — kahya consolidation show"
