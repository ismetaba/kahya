package consolidation

import "testing"

// TestByteExactTurkishStrings pins MsgLocalSkipped/MsgSuggestionReady to
// the exact bytes CLAUDE.md's language policy requires (both contain an
// em-dash "—", never a hyphen) - verified independently, once, against
// the actual task spec markdown during development; this test guards
// against a future accidental paraphrase/reflow.
func TestByteExactTurkishStrings(t *testing.T) {
	wantLocalSkipped := "yerel model için bellek yok — gizli-şerit dosyaları bu gece atlandı"
	if MsgLocalSkipped != wantLocalSkipped {
		t.Errorf("MsgLocalSkipped = %q, want %q", MsgLocalSkipped, wantLocalSkipped)
	}
	wantSuggestionReady := "Konsolidasyon önerisi hazır — kahya consolidation show"
	if MsgSuggestionReady != wantSuggestionReady {
		t.Errorf("MsgSuggestionReady = %q, want %q", MsgSuggestionReady, wantSuggestionReady)
	}
	// FIX-P-1 (handbook G06 P-1 text, byte-exact).
	wantBackupDirStaged := "Konsolidasyon durduruldu — yedek dizini git'e eklenmek üzereydi; ~/Kahya/.gitignore dosyasını kontrol et"
	if MsgBackupDirStaged != wantBackupDirStaged {
		t.Errorf("MsgBackupDirStaged = %q, want %q", MsgBackupDirStaged, wantBackupDirStaged)
	}
	// FIX-P-7 (handbook G06 P-7 text, byte-exact).
	wantRewriteRejected := "Konsolidasyon bu gece iptal edildi — model kendisine verilmeyen bir dosya yolu döndürdü"
	if MsgRewriteRejected != wantRewriteRejected {
		t.Errorf("MsgRewriteRejected = %q, want %q", MsgRewriteRejected, wantRewriteRejected)
	}
	// FIX-P-24 (handbook G06 P-24 text, byte-exact).
	wantRepoStateUnsafe := "Hafıza deposu (~/Kahya) beklenmedik git durumunda — konsolidasyon atlandı; 'git -C ~/Kahya status' ile kontrol et"
	if MsgRepoStateUnsafe != wantRepoStateUnsafe {
		t.Errorf("MsgRepoStateUnsafe = %q, want %q", MsgRepoStateUnsafe, wantRepoStateUnsafe)
	}
	// FIX-P-12 (handbook G06 P-12 text, byte-exact).
	wantAutoCommitFailedPending := "Otomatik konsolidasyon birleştirilemedi — öneri bekliyor: kahya consolidation show"
	if MsgAutoCommitFailedPending != wantAutoCommitFailedPending {
		t.Errorf("MsgAutoCommitFailedPending = %q, want %q", MsgAutoCommitFailedPending, wantAutoCommitFailedPending)
	}
}
