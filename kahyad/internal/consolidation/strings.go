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

// MsgBackupDirStaged is the Telegram notice sent when the nightly run
// refused to commit because a path under the backup dir was staged in
// ~/Kahya (FIX-P-1; byte-exact handbook text, em-dash).
const MsgBackupDirStaged = "Konsolidasyon durduruldu — yedek dizini git'e eklenmek üzereydi; ~/Kahya/.gitignore dosyasını kontrol et"

// MsgRewriteRejected is the Telegram notice sent when a lane's rewrite named
// a file it was not given and the night was rejected (FIX-P-7; byte-exact
// handbook text, em-dash). It deliberately names no path: a secret-lane
// file name must not leave the machine; the path is in the local ledger.
const MsgRewriteRejected = "Konsolidasyon bu gece iptal edildi — model kendisine verilmeyen bir dosya yolu döndürdü"
