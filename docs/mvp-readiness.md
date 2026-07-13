# Kâhya — MVP hazırlık kontrol listesi (W78-06)

Bu belge, **her §6 haftalık kabul kapısını** ve **her §9 "MVP tamamlandı"
kriterini** onu kanıtlayan somut kanıta (görev kimliği + test adı veya
`kahya`/`make` komutu) eşler. Bu, MVP'nin gerçek günlük kullanıma girebileceğini
söyleyen kapıdır; `kahya readiness` ile **yeniden koşulabilir**.

İki kanıt sınıfı vardır:

- **İnşa kapıları** — dogfood'a başlamadan ÖNCE şimdi yeşil olması gerekenler
  (`make readiness` = `make test lint invariants` + `kahya readiness
  --phase=start`).
- **Kullanım kapıları** — yalnız gerçek 2 haftalık dogfood penceresi SIRASINDA
  sağlanabilenler (`make readiness-complete` = aynısı + `kahya readiness
  --phase=complete`).

Kapı-değerlendirme yeri:

- **Kod-testleri** (`make test`/`make lint`/`make invariants`) — CI + yerel
  `make readiness` orkestrasyonu koşar.
- **Kaydedilmiş kanıt satırları** (retrieval / red-team / restore-drill) —
  `kahyad`'ın `GET /readiness` uç-noktası brain.db'yi salt-okunur
  (`query_only`) okur; `kahya readiness` bunu UDS üzerinden ince-istemci olarak
  gösterir.
- **Kullanım eşikleri** — W78-04 metrikleri (`GET /metrics` / `GET /readiness`),
  brain.db events defterinden.
- **Veri-kaybı olayı** — `docs/dogfood.md`'nin yapılandırılmış olay sütunu,
  `kahya readiness --phase=complete` tarafından ayrıştırılır (CLI'de).

---

## §6 haftalık kabul kapıları (inşa kapıları)

| §6 kapısı | Görev | Kanıt (test / komut) | Değerlendirme yeri |
| --- | --- | --- | --- |
| **W1–2** çekirdek enjeksiyon + `'evlerimizden'`→`'ev'` (trigram, elle-stem yok) + tek `trace_id` + her `<hafiza>` bloğu defterde | W12-10 | `tests/e2e/w12_gate_test.go` `TestW12Acceptance` (hermetik, `-tags e2e` — `make test` içinde); canlı: `make accept-w12` | `make test` |
| **W1–2** gömme hattı (Qwen3-Embedding, TR→EN çapraz-dil geri-getirme) | W12-11 | `kahyad/internal/mlxe2e` çapraz-dil kapısı (`make test-mlx`, canlı model) | `make test-mlx` |
| **W3** W2 = byte-exact Telegram diff onayı; W3 Telegram ile onaylanamaz, yalnız yazılı CLI "onayla"; gizli-şerit yerel onaya düşer; egress allowlist + konteyner `curl` atlatamaz; gizli-şerit buluta ulaşamaz | W3-10 | `tests/w3/gate_test.go`: `TestGate1_W2RequiresByteExactTelegramDiffApproval`, `TestGate2_W3NeverApprovedViaTelegramOnlyLocalOnayla`, `TestGate3_SecretLaneContentRoutesToLocalApprovalOnly`, `TestGate4_DockerCurlCannotBypassEgressAllowlist`, `TestGate5_SecretLaneCannotReachCloud` | `make test` |
| **W4** SIGKILL-devam çift-yürütme yok / çevrimdışı→devam veya açık bildirim / defter kurcalama uzak-çapaya karşı yakalanır | W4-07 | `tests/acceptance/w4` (`-tags acceptance`): `TestScenarioA_KillResumeNoDoubleExecution`, `TestScenarioB_OfflineThenReconnectCompletes`, `TestScenarioC_TamperDetectedAgainstRemoteAnchor`; canlı: `make accept-w4` | `make test` (ön-kapı) |
| **W5** 08:30 brifing tek-bildirim + `trace_id` / gece konsolidasyonu diff-commit / kirlenmiş oturum W-aracını reddeder, temiz geçer / ~20-soru geri-getirme regresyon yok | W5-05 | `TestW5GateSingleNotificationTraceIDThenDuplicateSkipped`, `TestW5GateConsolidationProducesDiffThenApproveCommitsAsKahyaAndReindexes`, `TestSameToolSameTargetDeniedTaintedAllowedClean`, `TestRunnerRunDetectsInjectedRegression` (`make test` W5-05 kapı bloğu) | `make test` |
| **W6** hold→konuş→transkript→görev tümü yerel / `⌥⎋` uzun görevde devam/yeniden-deneme YOK (daemon restart sonrası bile) / palet-aç→ilk-token events'e loglanır | W6-04 | `tests/acceptance/w6`: `TestW6Gate1VoiceLoopFullyLocal`, `TestW6Gate2HaltSurvivesDaemonRestart`, `TestW6Gate3PaletteAndFirstTokenLoggedToEvents` | `make test` (ön-kapı) |
| **W7–8** geri-getirme precision ≥%80 (çekimserlik dahil) | W78-01 | `kahyad/internal/eval` `TestPrecision`, `TestRetrievalRunnerUnanswerableFalsePositiveDropsPrecision`, gate testleri (`TestGateRefusesRed`/`Stale`/`WhenNoResult`); canlı drill: `make eval-retrieval` → `eval.retrieval.result` satırı | `make test` + kanıt satırı |
| **W7–8** red-team 0 atlatma (dev profili) | W78-02 | `kahyad/internal/eval` `TestRedteamAllScenariosBlocked`; canlı drill: `make eval-redteam` → `eval.redteam.result` satırı | `make test` + kanıt satırı |
| **W7–8** §5 değişmez testleri CI'da | W78-03 | `make invariants` (`tests/invariants/cmd/runinvariants`, `docs/coverage.md` §5 harita); `make test` içinde `TestCoverageMapAllTestsExist` | `make invariants` |
| **W7–8** metrikler | W78-04 | `kahya metrics` (`GET /metrics`); `kahyad/internal/metrics` `metrics_test.go` | `make test` + `make metrics` |
| **W7–8** yedek geri-yükleme tatbikatı bir kez | W78-05 | `kahyad/internal/restore` `TestRestoreDrillEquivalenceAndLedgerSurvival`; canlı: `make restore-drill` → `restore.drill.result` satırı | `make test` + kanıt satırı |
| **W7–8** dogfood hazırlık kapısı + takip defteri | W78-06 | `kahya readiness` (`GET /readiness`); `kahyad/internal/readiness` + `server` + `cmd/kahya` testleri; `make readiness` | `make readiness` |

---

## §9 "MVP tamamlandı" kriterleri

HANDOFF §9 (aynen): *"MVP tamamlandı sayılır: 2 hafta kesintisiz günlük kullanım ·
sıfır veri-kaybı olayı (yedekten geri-dönüş bir kez tatbik edildi) · ≥10
komut/gün · haftada ≥5 'hatırladı' anı · egress/gizli-şerit/W3 değişmezleri
kod-testli (CI'da yeşil)."*

| §9 kriteri | Kapı sınıfı | Görev | Kanıt (komut / test) | `readiness` alanı |
| --- | --- | --- | --- | --- |
| 2 hafta kesintisiz günlük kullanım | kullanım | W78-06 | `kahya readiness --phase=complete` (≥14 gün kayıtlı komut etkinliği); `docs/dogfood.md` doldurulmuş | `usage_gates.window_ok` |
| ≥10 komut/gün (sürdürülebilir) | kullanım | W78-04 | `kahya readiness --phase=complete` / `kahya metrics` (pencere ort. ≥10 **ve** ≥14 ayrı gün ≥10) | `usage_gates.commands_per_day_ok` |
| haftada ≥5 "hatırladı" anı | kullanım | W78-04 (W5-03 işaretleme akışı) | `kahya readiness --phase=complete` (hafta başına ort. ≥5) | `usage_gates.remembered_ok` |
| sıfır veri-kaybı olayı | kullanım | W78-06 | `kahya readiness --phase=complete` `docs/dogfood.md` olay sütununu ayrıştırır — hiç `type: data-loss` satırı yok | `usage_gates.data_loss_ok` |
| yedekten geri-dönüş bir kez tatbik edildi | inşa | W78-05 | son `restore.drill.result` `ok=true`; `make restore-drill` | `build_gates.restore_drill` |
| egress/gizli-şerit/W3 değişmezleri kod-testli (CI'da yeşil) | inşa | W78-03 (+ W3-10) | `make test` + `make invariants` (`docs/coverage.md` §5→test haritası) | `make readiness` orkestrasyonu |
| geri-getirme precision ≥%80 (inşa ön-koşulu) | inşa | W78-01 | son `eval.retrieval.result` `precision≥0.80`; `make eval-retrieval` | `build_gates.retrieval` |
| red-team 0 atlatma (inşa ön-koşulu) | inşa | W78-02 | son `eval.redteam.result` `bypasses=0`; `make eval-redteam` | `build_gates.redteam` |

### Kuzey-yıldızı hedefleri (RAPORLANIR, kapı DEĞİL)

HANDOFF §6 kuzey-yıldızı hedefleri — `kahya readiness` bunları pass/fail
işaretiyle **raporlar** ama çıkış kodunu **belirlemez** (§9 sözleşmedir):

| Hedef | Değer | `readiness` alanı |
| --- | --- | --- |
| komutların ≥%60'ı açıklama-turu olmadan tamamlanır (açıklama-turu oranı ≤%40) | `northstar.clarification_turn_rate` (W78-07'den beri canlı: kahyad, worker'ın açıklama-turu sinyali üzerine `kind="clarification_turn"` event yazar — boş/yeni bir ledger'da hâlâ veri-yok) | `northstar.clarification_ok` |
| palet-aç→ilk-token p50 <1.5s | `northstar.palette_first_token_p50_ms` | `northstar.palette_ok` |

---

## Nasıl koşulur

```
# İnşa kapıları (dogfood'a başlamadan önce) — çalışan bir kahyad + kaydedilmiş
# retrieval/red-team/restore-drill kanıt satırları gerektirir:
make readiness

# MVP-tamamlandı kapısı (gerçek 2 haftalık dogfood penceresinden SONRA;
# docs/dogfood.md doldurulmuş olmalı) — görev zamanında BEKLENEN kırmızıdır:
make readiness-complete
```

`kahya readiness` yalnız kahyad UDS `GET /readiness` üzerinden konuşur; brain.db'yi
kendisi asla açmaz (§4 kilitli karar: tek db-erişim yolu). Kapı MANTIĞI (inşa
yeşil/kırmızı/eksik, kullanım kırmızı-sonra-yeşil, olay ayrıştırma) fikstürlerle
hermetik olarak `make test` içinde kanıtlanır — daemon gerektirmez.

> **Kural (HANDOFF §9):** MVP 2 hafta gerçek günlük kullanımdan sağ çıkmadan
> v1'den hiçbir şey başlamaz. Takip: [`docs/dogfood.md`](./dogfood.md).
