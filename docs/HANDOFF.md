# Kâhya — İnşa Handoff'u

> Bu belge, **Kâhya** projesini devralan bir geliştirici (ya da yeni bir Claude Code oturumu) için yazılmıştır. Kendine yeter; önceki sohbete ihtiyaç duymadan inşaya başlayabilirsin. Sıraya göre oku: **Görev → Durum → Bağlam → Kilitli Kararlar → Değişmezler → MVP Yolu → Gün 1**.
>
> Tam tasarım dokümanı (mimari diyagram, senaryolar, tablolar): https://claude.ai/code/artifact/466f3b05-4443-4ba9-ab8c-02ee17ab315f — **ve** Gün 1'de repoya `docs/design.html` olarak commit edilir (link ölürse belge kendine yetmeli).
> Hafıza kaydı: `~/.claude/projects/-Users-matt-Test/memory/kahya-ai-brain.md`

> **v0.2 — bu sürümde ne değişti.** Belge profesyonel bir mimari incelemeden (7 mercek × hasım-doğrulama, 46 doğrulanmış bulgu) geçti. Kilitli kararlar ve vizyon aynı; eklenen şey **inşa edilebilirlik**: IPC sözleşmesi, güvenlik değişmezlerinin uygulama düzlemi (yalnız `can_use_tool` değil), gizli-şerit/egress çatışmalarının çözümü, eksik şema tabloları, migrasyon/yedekleme/izin/bellek operasyonu, ölçülebilir kabul kriterleri ve §5 değişmezlerinin haftalara dağıtılması. Düzeltmeler ⚑ ile işaretli.

---

## 1 · Görev

**Kâhya**, tek bir güç-kullanıcısı için, yerel-öncelikli ve Türkçe-öncelikli bir kişisel AI asistanıdır. Üç sütun:

1. **Yaşayan Hafıza** — sana söylenen/gözlemlenen her şey saklanır, organize edilir, yıllar içinde iyileşir. Diskte kullanıcının sahip olduğu, göz-le-düzenlenebilir bir külliyat.
2. **Mac'in Dümeni** — kullanıcının gerçek Mac'ini (mail, terminal, dosya, uygulama) onun adına kullanır; kazanılan otonomi + geri-alma + değiştirilemez defter altında.
3. **Proaktiflik** — zamanlayıcı ve gözcülerle sohbeti kendisi başlatır (sabah brifingi, "CI kırmızı", "pasaportun doluyor").

Metafor: Osmanlı **kâhya**sı — evi sahibinin adına *kendi başına* yönetir, güveni yıllarca kazanır. Tüm güvenlik mimarisi buna oturur.

---

## 2 · Durum

- **Greenfield.** Henüz kod yok. Tasarım tamam ve kilitli (5-mimar + 3-hasım-eleştirmen sürecinden geçti; ardından mimari inceleme + hasım-doğrulama).
- Bir sonraki iş: **8 haftalık MVP kritik yolu** (§6).
- Kural: **MVP 2 hafta gerçek günlük kullanımdan sağ çıkmadan v1'den hiçbir şey başlamaz.**

---

## 3 · Bilmen gereken bağlam (kullanıcı)

- Kıdemli **backend geliştirici**: Go mikroservisleri, NATS event-driven mimari, saga desenleri, `trace_id`/correlation gözlemlenebilirliği. Tasarım kasıtlı olarak bu zihinsel modele oturur.
- Yerel AI yığınlarını zaten çalıştırıyor: MLX, llama.cpp, LM Studio, ComfyUI, Whisper-sınıfı modeller. Python + Go rahat.
- **Donanım:** Apple **M5 Max, 128GB** unified memory, macOS (Darwin 25, orta-2026). Not: ComfyUI/Wan video yığınını da çalıştırıyor → yerel model bellek bütçesine dikkat (bellek baskısı mekanizması §4'te tanımlı).
- **Var olan varlıklar (gün 1'de kullan):**
  - `~/Project 1/gold-token` — altın custody Go mikroservisleri (EVM tx + NATS + saga + trace_id). Hafıza tohumu ve devops senaryoları için gerçek bağlam.
  - `~/.claude/projects/-Users-matt-Test/memory/MEMORY.md` + proje notları — **hafızayı gün 1'de bununla tohumla** (soğuk-başlangıç, terk edilmenin 1 numaralı nedeni). Tohum tier eşlemesi §7'de.
- **Dil politikası:** Sohbet/UI Türkçe-öncelikli; teknik çıktı (kod, log, model ID) İngilizce. Hafıza dil-agnostik saklanır (geldiği dilde), çok-dilli gömülü ile TR sorgu EN notu bulur.

---

## 4 · Kilitli kararlar (yeniden tartışmaya kapalı)

> ⚑ Bu kararlar 5-mimar + 3-hasım-eleştirmen sürecinden geçti. Yeniden tartışma; yalnız içinde somut bir kusur, iç çelişki, teknik yanlışlık veya uygulanamazlık bulursan işaretle.

### Mimari — iki süreç, tek omurga
- **`kahyad` (Go daemon)** — kontrol düzlemi. launchd LaunchAgent (`KeepAlive=true`). Sahip olduğu: niyet yönlendirici, görev/saga durum makinesi (kademeli yürütme), politika motoru, maliyet valisi, defter (append-only events), zamanlayıcı, ve **SQLite hafıza indeksi**. **Keychain'den bulut anahtarını okuyan tek süreç.**
- **Agent SDK worker (Python)** — tüm akıl yürütme. `claude-agent-sdk`. `kahyad` tarafından spawn edilir. Araç döngüsü, bağlam sıkıştırma, alt-ajanlar, MCP istemcisi, oturum devamı buradan bedava gelir. **Yeniden yazma.**
- **MLX yardımcı süreç (Python)** — gömülü + Whisper + gizli-şerit yerel modeli. `mlx_lm.server` / `mlx-whisper`.
- **UI (MVP):** **`kahya` CLI** (tek-atış + REPL + `kahya log --trace <id>`; daemon'a UDS üzerinden konuşur — **W1–5'in birincil etkileşim yüzeyi budur, palet W6'da gelir**) + Hammerspoon (global kısayol + onay kartı + `⌥⎋` acil durdurma) + **Telegram botu** (uzaktan onay, yalnız geri-alınabilir eylemler). **SwiftUI menü-çubuğu app v2 işi** — MVP'de yok.

⚑ **IPC sözleşmesi (§ inşa için kritik, W1–2'de sabitlenir):**
- kahyad worker'ı **görev-başına** spawn eder: görev zarfı JSON stdin'den; `trace_id` env/arg ile geçer; W4 oturum devamı `session_id` ile.
- Worker `ClaudeSDKClient` + **streaming input modu** üzerine kurulur — `UserPromptSubmit` kancası ve `can_use_tool` geri-çağrısı tek-atımlık `query()` ile ÇALIŞMAZ.
- Politika kontrolü: `~/Library/Application Support/Kahya/kahyad.sock` üzerinden **HTTP-over-UDS** `POST /policy/check`, timeout 5s; **her hata/timeout = RED (fail-closed)** — §5 "güvenlik yürütücüde" ilkesinin doğal sonucu.
- MLX süreçlerini **kahyad süpervize eder** (launchd yalnız kahyad'ı tutar). `mlx_lm.server` = 127.0.0.1'e bağlı **TCP HTTP** sunucusu (OpenAI-uyumlu, varsayılan :8080, auth yok); kahyad portu config'te sabitler ve health-check yapar. `mlx-whisper` bir sunucu değil, worker içinde **kütüphane** olarak.
- **API anahtarı worker'a verilmez:** kahyad localhost'ta auth-header ekleyen bir **forward-proxy** dinler ve worker'ı `ANTHROPIC_BASE_URL=http://127.0.0.1:<port>` ile spawn eder. **Maliyet valisi, cache-hit metriği ve model-çağrısı egress kapısı bu proxy noktasında uygulanır.**
- **Bildirim teslimi:** yerelde Hammerspoon `hs.notify` (eylem düğmeleriyle), uzaktayken Telegram; arka-plan görev sonuçları aynı kanaldan `trace_id` ile döner.
- Tüm süreçler her satırda `trace_id` içeren **JSONL** loglar — W1–2 kabul kriteri ancak böyle ölçülebilir.

### Eylem sınıfları & otonomi merdiveni
⚑ **Eylem sınıfları** (§5 ve §6'da bunlarla anılır — ilk kullanımdan önce tanımlı):
- **R** = salt-okuma · **W1** = geri-alınabilir yazma · **W2** = sert/zor geri alınır yazma · **W3** = geri dönüşsüz (para · prod · kimlik · senin adına mesaj)

⚑ **Otonomi merdiveni** (her *araç × sınıf × alan* üçlüsü için ayrı kazanılır):

| Seviye | Ad | Otomatik | Onaylı |
|---|---|---|---|
| L0 | Gözlemci | — | her şey |
| L1 | Çırak | R | W1/W2/W3 |
| L2 | Eşlikçi | R, W1 (5-dk geri-alma + defter) | W2/W3 |
| L3 | Vekil | R, W1, W2 (kazanılmış allowlist) | W3 |
| L4 | Kâhya | R, W1, W2 | **W3 (daima)** |

- **Terfi:** eylem-sınıfı başına 20 ardışık onay + 0 red → ürün terfi **ÖNERİR**; kullanıcı onaylamadan asla otomatik terfi olmaz.
- **Tenzil:** red / geri-alma / güvenlik ihlali → bir seviye düşer.
- **W3 her seviyede, sonsuza dek kalıcı yazılı onay ister.** Bu bir ayar değil, ürün ilkesi.
- **Geri-alma (undo):** policy.yaml araç kaydında `reversible: true/false` + araç-başına undo tarifi (silme→Çöp, dosya yazımı→işlem-öncesi git checkpoint, mail→taslak-asla-gönderme). "Geri-alınabilir" sınıflandırması Telegram onay kapısını (§5 #5) ve W1 5-dk penceresini besler.

### Yığın
| Katman | Seçim |
|---|---|
| Kontrol düzlemi | **Go** + `sqlc` üretimli sorgular. ⚑ **Migrasyon: `goose`/`golang-migrate`** (sqlc migrasyon yapmaz), kahyad açılışında her işten önce koşar; sürüm `PRAGMA user_version` |
| Ajan runtime | **Python** + `claude-agent-sdk` (sürüm sabitlenir + lock dosyası) |
| Araç protokolü | **MCP** (%100 entegrasyon — kendi araçların dahil) |
| Veritabanı | Tek **SQLite** (WAL) + `sqlite-vec` (≥0.1.9 pinle; brute-force KNN, ≤~100k vektörde yeterli) + FTS5. ⚑ **FTS5 çift indeks:** `tokenize='trigram'` (Türkçe ek/İ-ı duyarsız kısmi eşleşme) + `unicode61` (kesin terim/kod); BM25 skorları füzyonda birleşir. Şifreleme: FileVault + Keychain (SQLCipher **değil**) |
| Hafıza kaynak gerçeği | **Markdown + git** (`~/Kahya/memory/*.md`); SQLite türetilmiş indeks |
| Zamanlama | ⚑ Duvar-saati işleri (08:30 brifingi, gecelik konsolidasyon) **launchd `StartCalendarInterval`** ile (uykuda kaçarsa uyanışta bir kez koşar); daemon-içi `robfig/cron` yalnız daemon-çalışırken kısa-aralık iç tick'ler için (Go darwin monotonic saati uykuda durur, golang/go#24595 → duvar-saati işlerine güvenme) |
| İç veri yolu | MVP: SQLite outbox tablosu + goroutine'ler. **Gömülü NATS = v2** |
| STT | `whisper-large-v3-turbo` (mlx-whisper, `language=tr` sabit), push-to-talk |
| TTS | `say -v Yelda` (MVP). Piper/XTTS ertelendi. ⚑ Yelda sesi kutudan gelmeyebilir — Gün 1 kurulumda `say -v '?'` ile doğrula, yoksa Sistem Ayarları'ndan indir |

### Model yönlendirme (karar **Go kodunda**, istemde değil)
| Görev | Model | Fiyat /MTok |
|---|---|---|
| Planlama · zor yürütme · çok-dosya kod | `claude-opus-4-8` | $5 / $25 (1M ctx) |
| Alt-ajan yürütme · fan-out | `claude-sonnet-5` | $3 / $15 (*intro $2/$10, 31.08.2026'ya dek*) |
| **Yönlendirme / sınıflandırma** | ⚑ **yerel Qwen3-30B-A3B** (<300ms) — gizli-şerit tespiti burada | — |
| Çıkarım · geri-yazım (gizli-şerit-dışı onaylı) | `claude-haiku-4-5` | $1 / $5 |
| **Okuyucu** (güvenilmez içerik ayrıştırma) | ⚑ içerik yerel ön-sınıflandırıcıdan geçer; gizli-şerit ise **Qwen3-30B-A3B (yerel)**, değilse `claude-haiku-4-5` | — |
| **Gizli şerit** (finans/sağlık/kimlik) | `Qwen3-30B-A3B` MLX 4-bit yerel | — (makineden çıkmaz) |
| "Derin düşün" opt-in | `claude-fable-5` | $10 / $50 |

⚑ **Sıralama değişmezi:** *Hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez.* policy.yaml globları **yalnız dosya yolları** için; mail/web gibi içerik-kaynaklı veride gizli-şerit kararı yerel içerik-sınıflandırıcıyla **alım anında** verilir.

- Fable 5 **asla varsayılan değil** ve daima `betas:["server-side-fallback-2026-06-01"]` + `fallbacks:[{model:"claude-opus-4-8"}]` ile (30-gün saklama zorunluluğu + güvenlik-bitişik işte red sınıflandırıcıları).
- Yerel filo v1'de **tam üç** yerleşik model: `whisper-large-v3-turbo`, `Qwen3-Embedding-0.6B` (512-dim MRL, `model_ver` etiketli), `Qwen3-30B-A3B`. Reranker/120B/wake-word ertelendi.
  - ⚑ **`model_ver` kullanım kuralı:** her vektör satırı `model_ver` taşır; KNN sorguları daima **tek aktif `model_ver`'e filtrelenir** (karışık-versiyon KNN yasak). Gömülü yükseltme = Markdown kaynak-gerçeğinden **tam yeniden-gömme**; §5-Hafıza-#5 retrieval eval kapısı yeşilse aktif versiyon değişir, eski vektörler sonra silinir.

⚑ **Maliyet valisi (somut):** görev-başına 500K token tavanı; günlük bütçe $10 / aylık $150. Tavanda görev **duraklar** + Telegram bildirimi; günlük bütçenin %80'inde yönlendirici bir kademe ucuza düşer (Opus→Sonnet→yerel). Cache-hit oranı ve günlük harcama **alarm verir** (Telegram'a) — sessiz cache-bozan maliyeti 5–10× katlar. İstem önbelleği: donmuş sistem-öneki + araç tanımları, 1-saat TTL.

⚑ **Bellek baskısı:** Qwen3-30B-A3B (~17GB) **talep-üzerine yüklenir, boşta-TTL ile boşaltılır**; kahyad yüklemeden önce kullanılabilir belleği kontrol eder. Yetersizse gizli şerit **FAIL-CLOSED** — kullanıcıya "yerel model için bellek yok" der, **ASLA buluta yönlendirmez** (§5 yerel-yalnız değişmezinin operasyonel karşılığı; ComfyUI/Wan yan yana çalışırken kritik).

---

## 5 · Değişmezler — GÜN 1'DE ŞEMAYA GÖMÜLMELİ

Bunlar mimari değil, **şema-ve-politika** seviyesinde. Erken eklemek ucuz, sonradan eklemek felaket. Üç eleştirmen de bunlarda uzlaştı. ⚑ Her değişmezin hangi hafta inşa edildiği ve kod-testi §6'da bir kabul kriterine bağlıdır.

### Güvenlik (yürütücüde uygula, asla istemde)

⚑ **Uygulama düzlemi (önce oku):** `can_use_tool` bir **erken-ret/UX katmanıdır, güvenlik sınırı değildir** — worker sürecinin içinde çalışan bir SDK geri-çağrısıdır. Bağlayıcı politika kararı **kahyad'da** verilir; yan-etkili MCP araçları kahyad'ın verdiği **tek-kullanımlık onay jetonunu** doğrulamadan yürümez (ya da yan-etkili MCP sunucularını kahyad spawn edip sahiplenir, worker onlara yalnız kahyad üzerinden erişir).

1. **Egress birinci-sınıf kapılı bir yetenek.** Off-box'a byte gönderen her çağrı (HTTP gövde *ve* URL, DNS, mail, panoya-uzak) hedef **allowlist** + hacim bütçesine tabi. Aynı oturumda hassas okuma varsa allowlist-dışı egress sert bloke.
   - ⚑ **Model-yazımı shell konteyneri varsayılan `--network none`;** ağ gerektiren işler yalnız kahyad'ın egress proxy'si (allowlist + hacim bütçesi aynı noktada) üzerinden çıkar — aksi hâlde container içi `curl` allowlist'i atlar.
   - ⚑ **Onay kartları egress sayılır ve aynı kapıdan geçer.** Allowlist-*içi* ama içerik-taşıyabilen hedefler (Telegram `sendMessage`, `gh` yazma uçları) da hassas-okuma-sonrası içerik kısıtına tabidir.
2. **Veri-seviyesi taint + ikili-LLM.** Güvenilmez baytları **araçsız "Okuyucu"** ajan işler, yalnız yapılandırılmış-doğrulanmış veri döndürür. Yetkili **"Eylemci"** ham güvenilmez metni asla görmez. Bağlama giren herhangi güvenilmez içerik oturumu ömür boyu güvenilmez katmana düşürür.
   - ⚑ **Operasyonel tanım:** Güvenilmez katman = **yalnız R-sınıfı araçlar + kullanıcıya bildirim**. Her W-eylemi (W1 dahil, örn. Outlook taslağı) **TEMİZ yeni bir Eylemci oturumunda**, yalnız Okuyucu'nun Go-tarafında struct/şema-doğrulanmış çıktısıyla (serbest-metin alanları uzunluk+karakter-sınıfı kısıtlı) tohumlanarak yürütülür. Sabah brifingi tasarım gereği güvenilmezdir → yalnız bildirim üretir.
   - ⚑ **Taint kalıcılığı:** taint katmanı `session_id` anahtarıyla SQLite'ta (tasks/sessions) **kalıcı saklanır**, resume'da yeniden yüklenir ve **yalnız yükselir — asla düşmez**; kayıt yoksa oturum güvenilmez sayılır (fail-closed).
3. **`source_tier` + profil-kartı insan kapısı.** Güvenilmez-alınmış hafıza profil kartına giremez / refleks enjekte edilemez.
4. **Dış-çapalı defter.** Ayrıcalıklı taraf tek denetim otoritesi; zincir başı daemon'ın yeniden-yazamayacağı ayrı-yetkili bir depoya periyodik yazılır. Her model çağrısındaki enjekte `<hafiza>` bloğu kaydedilir (zehirlenme adli izlenebilirliği).
   - ⚑ **Somut mekanizma (W4):** kahyad her N saatte defterin son event hash'ini **yalnız-append yetkili ayrı uzak hedefe** yazar (force-push kapalı, append-only deploy-key'li ayrı git repo'su, ya da S3 Object Lock; offline'da farklı-uid'li yerel ekleme-yalnız dosya). Bu kimlik Keychain'de **ayrı öğedir**, yalnız çapa-yazma kod yolunda okunur.
5. **WYSIWYE onay.** NFC-normalize, bidi/sıfır-genişlik/homoglyph temizliği, kanonik yol/host, onaylanan baytın hash'i — yürütülen bayt farklıysa **ret**.
   - ⚑ **W3 yazılı "onayla" YALNIZ yerel yüzeyden kabul edilir** (W3–W5: CLI istemi; W6+: Hammerspoon kartı). Telegram W3 için yalnız "yerelde onay bekleniyor" bildirimi gönderir, **onay girdisi kabul etmez**.
   - ⚑ **Gizli-şerit (finans/sağlık/kimlik) etiketli tek bir bayt içeren diff Telegram'a gönderilmez** — bu onaylar yalnız yerel yüzeyde gösterilir, Telegram'a en fazla redakte başlık gider.
   - ⚑ **Telegram auth:** tek sabit `chat_id`/`user_id` allowlist'i **Go tarafında (kahyad)** uygulanır — eşleşmeyen her update sessizce düşer + deftere loglanır; **long-polling** (gelen ağ yüzeyi yok); Telegram-kaynaklı onaylar defterde `remote` etiketli. Token Keychain'de (§9 kapsıyor).
6. **Shell:** ikili-allowlist güvenlik sınırı **değil** (`git -c`, `find -exec`, `tar --checkpoint-action` = keyfi kod). Tüm model-yazımı shell **varsayılan Docker'da** (yalnız görevin açık iş-dizini rw bind-mount, gerisi yok/ro; ağ kapalı); host yürütme yalnız arg-doğrulamalı dar bir set.
   - ⚑ **`osascript`/JXA/Shortcuts gövdeleri shell ile aynı 'keyfi kod' sınıfıdır** — statik etiketi **en az W2**, script baytları WYSIWYE diff'iyle onaylanır; `do shell script`/`doShellScript` içeren gövdeler reddedilir veya Docker-shell aracına yönlendirilir.
   - ⚑ **`fs` yazma-deny globları (policy.yaml, Gün 1):** `~/.zshrc` ve shell rc/profil dosyaları, `~/Library/LaunchAgents/**`, `~/.hammerspoon/**`, `~/Library/Application Support/Kahya/**` (defter/DB'nin kendi kendini kurcalamasına karşı).

### Hafıza doğruluğu (aksi hâlde 12–24 ayda sessizce çürür)
1. **Kaynak-güven kafesi:** her olgu `source_tier` ∈ {`user_edit`(1.0) › `user_asserted`(≤.95) › `external_doc`(≤.8) › `screen`(≤.7) › `agent_derived`(≤.4)}. Ajan-türevi karantinada, kullanıcı onaylayana dek profil kartından/enjeksiyondan hariç.
2. **Bölünebilir, kanıt-kapılı varlık birleştirme:** isim benzerliğiyle asla oto-birleştirme (Türkçe'de sayısız Emre/Ahmet). En az bir ayırt edici kanıt şart. Merge-defteri + **varlık-bölme** operasyonu. Şüpheli aynı-isim → yeni geçici varlık.
3. **Negatif kanıt + log-odds güven:** noisy-OR ratchet yok; aynı-oturum tekrarı tek kanıt sayılır. Kullanıcı reddi/başarısız tazelik-yoklaması güveni düşürür; <0.3 enjeksiyondan çıkar. "Artık sevmiyorum" → geri-çekme.
4. **90+ gün sıcak pencere + ayrıntı-atomu:** 48 saat değil ≥90 gün. Soğutmadan önce sayı/tarih/alıntı/karar/söz'ler yapılandırılmış olgulara terfi. Her özet **ham kanıttan** üretilir, asla alt-özetten.
5. **Gerçek-temelli değerlendirme:** değerlendirme kümesi mağazanın kendi inançlarından değil, **haftalık doğru/yanlış ritüelinin insan etiketlerinden** beslenir. 1/6/24-ay ayrıntı-yoklamaları + precision + çekimserlik. Her konsolidasyon/gömülü/füzyon değişikliğinden **önce** kapı.

⚑ **Şema — W1–2'de açılması ZORUNLU tablolar** (yukarıdaki #2–#4 sonradan türetilemez; kanıt↔oturum bağı yakalama-anı verisidir):
```
episodes, chunks,
facts (subject, predicate, object, source_tier, evidentiality [-mış morfolojisi:
       witnessed|reported|inferred], confidence [log-odds], importance,
       valid_from/to, status, evidence, extractor_ver),
entities + entity_aliases,
evidence (fact_id, episode_id, session_id, polarity ±),
merge_ledger (birleştirme/bölme kayıtları),
tasks, events/outbox
```
İki-zamanlı sorgular (valid_from/to) MVP'de kolon olarak var ama tam graf sorgusu v2 (§8). Tablolar boş başlayabilir; kritik olan **gün 1'de var olmaları**.

### Ürün ilkeleri (kalıcı, ayar değil)
- **W3 sınıfı** (para · prod altyapı · kimlik/parola · senin adına mesaj) her otonomi seviyesinde **kalıcı yazılı "onayla"** ister. Ürün asla sessizce satın almaz/göndermez.
- Güvenlik **yürütücüde**: eylem-sınıfı etiketleri araç kayıtlarında statik metadata; otonomi matrisi araç çalışmadan *önce* Go'da kontrol. Modeli anahtarları olmayan parlak-ama-fazla-özgüvenli bir junior gibi ele al.
- Gizlilik **kodda**: `finans/sağlık/kimlik` → yerel-yalnız, hiçbir model çıktısı/enjeksiyonun geçemeyeceği Go dalı + UI'da "yerel işlendi" rozeti.
- Hafıza bir izni asla düşüremez; tercihler işin *nasıl* önerileceğini bilgilendirir, *yapılıp yapılmayacağını* değil.

---

## 6 · MVP kritik yolu (8 hafta, yarı-zamanlı ~10–15 sa/hafta)

Her hafta bir kabul kriteriyle biter. ⚑ **Zamanlama notu:** W1–2 kapsamı yüklüdür; **W1–2 kabulü FTS5-only aramayla sağlanır** — embedding hattı (MLX süreç + Qwen3-Embedding-0.6B servisi + chunk gömme) ayrı iş kalemidir, sığmazsa W3–4'e kayar (şemadaki embedding kolonları + `model_ver` yine gün 1'de açılır; çok-dilli TR→EN retrieval embedding hattı bitene dek çalışmaz). W3 en riskli hafta olduğundan AppleScript/JXA/Shortcuts W4–W5'e kaydırılabilir.

- **W1–2 · Çekirdek.** `kahyad` iskeleti (Go, launchd), **goose migrasyonları** + tek SQLite dosyası + tam şema (yukarıdaki tablo listesi, `PRAGMA busy_timeout` + WAL checkpoint politikası), `sqlite-vec` + **FTS5 çift indeks (trigram + unicode61)**, hafıza MCP sunucusu (`memory_search` / `memory_write` / `memory_forget`, **kahyad içinde Go — brain.db'nin TEK yazarı kahyad'dır**), Agent SDK `UserPromptSubmit` enjeksiyon kancası (`ClaudeSDKClient` streaming), IPC sözleşmesi (§4). → **Kabul:** CLI'dan sorulan bir soru, tohumlanmış hafızadan bir `<hafiza>` bloğu enjekte edip yanıtlanıyor; **`'evlerimizden'` sorgusu `'ev'` içeren tohum notu buluyor** (Türkçe morfoloji); her şey tek `trace_id` taşıyor (JSONL loglarda doğrulanır).
- **W3 · Politika + araçlar.** `policy.yaml` (R/W1/W2/W3 statik etiketler + `reversible` + gizli-şerit globları + fs-deny globları), `can_use_tool` → `kahyad` kapısı (fail-closed), `fs`/`shell`(Docker: **runtime kurulumu — colima/Docker Desktop — + sandbox imajı + mount politikası + `network=none`**)/AppleScript/JXA/Shortcuts MCP araçları, Telegram onay botu (chat_id allowlist, inline butonlar), **gizli-şerit Go yönlendirme dalı + Qwen3-30B MLX entegrasyonu**. → **Kabul:** W2 bir eylem Telegram'dan byte-tam diff ile onay istiyor; W3 eylem **Telegram'dan onaylanamıyor, CLI'dan yazılı "onayla" ile geçiyor**; gizli-şerit dokunuşlu eylemler yerel onaya düşüyor; egress allowlist devrede + **container içi `curl` allowlist'i atlayamıyor (test)**; gizli-şerit içerik bulut çağrısına çıkamıyor (test).
- **W4 · Dayanıklılık.** launchd (`StartCalendarInterval`) + daemon-içi cron, arka-plan görev + oturum devamı (`session_id` ile resume), **idempotency/makbuz semantiği** (`intent → executing → receipt`; makbuzsuz `executing`'de yalnız W1 oto-tekrar, W2/W3 asla), **Okuyucu/Eylemci taint ayrımı** (web/mail girdileri), **bulut çağrı hata taksonomisi** (retryable 429/5xx/ağ + üstel backoff + max deneme + `bekliyor-yeniden-deneme` durumu), **dış-çapalı defter push'u** (§5 #4), **yedekleme** (aşağıda). → **Kabul:** ≥10 dk süren, en az bir W2 aracı çağıran görev bir araç çağrısı ortasında SIGKILL edilir; resume sonrası **çift araç-yürütmesi olmadan** (outbox/defter kayıtlarıyla doğrulanır) tamamlanıyor; ağ kapalıyken verilen komut ağ dönünce tamamlanıyor ya da açık hata bildirimiyle kapanıyor; defter yerelden değiştirilince uzak çapayla uyumsuzluk tespit edilip alarm veriyor.
- **W5 · Proaktiflik + konsolidasyon.** Sabah brifingi rutini (Graph olmadan: `gh`/dosya/takvim yerel), gecelik konsolidasyon (bir Agent SDK oturumu markdown'ı birleştirip git-commit eder — **yalnız markdown+git'e yazar, SQLite reindex'ini kahyad tetikler**). ⚑ **Konsolidasyon ilk 2 hafta öneri-modunda:** diff üretir, kullanıcı onayıyla commit eder; otomatik commit W7 mini-eval yeşiliyle açılır. ⚑ **Commit disiplini:** konsolidasyondan önce kirli working tree `author=user` commit'i olur (user_edit tier git author'dan türetilir); daemon değişiklikleri daima `author=kahyad` ayrı commit; çelişkide user_edit kazanır (daemon o gün kullanıcının dokunduğu satırları atlar). ⚑ **Haftalık doğru/yanlış ritüelinin hafif sürümü** (Telegram'dan ~10 olguluk "bu doğru mu?", W3 botunu yeniden kullanır) W5'te başlar; W7–8 eval kümesinin etiketleri buradan gelir (§5-H5). → **Kabul:** 08:30 brifingi tek bildirim + `trace_id`; gece külliyat konsolide olup diff commit ediliyor; **tainted brifing oturumundan doğrudan W-araç çağrısı reddediliyor** (aynı eylem temiz oturumdan geçiyor); ~20 soruluk retrieval mini-baseline konsolidasyon sonrası gerileme göstermiyor.
- **W6 · Ses + kısayol.** Hammerspoon `⌥Space` palet + `⌥⎋` acil durdurma, PTT → `mlx-whisper` (`language=tr`). ⚑ **`⌥⎋` semantiği:** Hammerspoon'dan kahyad'a 'halt' IPC → worker process-group'u + ilgili Docker konteynerleri öldürülür, görev terminal `user_halted` durumuna yazılır (**session-resume ve outbox retry'dan kalıcı hariç**), bekleyen tüm onaylar geçersiz kılınır. → **Kabul:** basılı-tut → konuş → transkript → görev döngüsü, %100 yerel; **uzun görev sırasında `⌥⎋` → daemon yeniden başlasa bile görev devam ETMİYOR ve retry edilmiyor**; palet-aç→ilk-token zaman damgaları events tablosuna loglanıyor.
- **W7–8 · Sağlamlaştırma + değerlendirme.** ~50 gerçek Türkçe/karışık-dil komut + retrieval QA değerlendirme kümesi (etiketler W5 ritüelinden); kırmızı-takım eval seti (planlı-mail-profil-zehirler, web-sayfası-hafıza-sızdırır, homoglyph-onay-atlar, **tainted-oturum-restart-sonrası-hâlâ-tainted**); **§5 değişmez kod-testleri CI'da**; **metrik sorgusu/CLI** (events tablosundan komut/gün, açıklama-turu oranı, p50). ⚑ Kırmızı-takım evali yalnız **`KAHYA_ENV=dev` profilinde** koşar (ayrı brain.db + ayrı `~/Kahya-dev/memory` + egress deny-all + ayrı launchd etiketi + record-replay SDK fixture'ları). → **Kabul:** retrieval QA precision ≥%80 (çekimserlik dahil); kırmızı-takım setinde **0 başarılı atlatma**; tüm §5 değişmez testleri yeşil; bir kez yedekten geri-dönüş tatbik edildi (temiz makinede aynı sorguya aynı `<hafiza>` enjeksiyonu). Sonra 2 hafta gerçek günlük dogfood başlıyor.

⚑ **Yedekleme (W4 iş kalemi — "sıfır veri-kaybı" kabul kriterinin gereği):** (1) `~/Kahya` → private git remote; W5 gecelik commit'in sonuna `git push`; (2) gecelik `VACUUM INTO ~/Kahya/backups/brain-YYYYMMDD.db` + `PRAGMA integrity_check` (son 7 kopya; canlı WAL db Time Machine'den dışlanır, VACUUM kopyası dahil edilir) — **defter/episodes markdown'dan türetilemez, tek kopyadır**; (3) Keychain sırları kayıpta API-key rotasyonuyla yeniden üretilir.

⚑ **Metrik tanımları (dipnot):** *açıklama-turu* = asistanın eylemden önce kullanıcıya soru sorduğu tur; *hatırladı anı* = kullanıcının o oturumda açıkça vermediği bir hafıza olgusunun yanıtta doğru kullanımı (kullanıcı elle işaretler, haftalık ritüele bağlanır); *palet-aç→ilk-token* = palet açılış zaman damgası → ilk stream token'ı.

**Kuzey-yıldızı (MVP):** komut/gün — *yararlı mı?* (hedef: hafta 2'de ≥10/gün; komutların ≥%60'ı açıklama-turu olmadan tamamlanır; palet-aç→ilk-token p50 <1.5s).

---

## 7 · Gün 1 — somut kickoff

⚑ **Dizin adları ASCII** (`~/Kahya`) — non-ASCII `â` policy.yaml globlarında ve Docker/SQLite bayt-düzeyi karşılaştırmalarında NFC/NFD sessiz uyuşmazlık riski taşır; "Kâhya" yalnızca ürün/görünen ad. ⚑ **Kod ayrı repoda** (`~/code/kahya`), `~/Kahya` yalnız hafıza git'i (konsolidasyon commit'leri kod geçmişine karışmasın).

```bash
# 1) Hafıza deposunu kur ve mevcut notları tohumla
mkdir -p ~/Kahya/memory ~/Kahya/backups && cd ~/Kahya && git init
git remote add origin <private-remote>            # ⚑ yedekleme için, gün 1
# Tohumla — MEMORY.md indeksini ATLA (yalnız gerçek hafıza dosyaları):
rsync -a --exclude='MEMORY.md' \
  ~/.claude/projects/-Users-matt-Test/memory/ ~/Kahya/memory/
# gold-token README/notları da tohuma ekle (kişi/proje/topoloji bağlamı)
git add -A && git commit -m "seed: import existing memory corpus"   # ⚑ ilk commit
```
⚑ **Tohum tier eşlemesi:** tohum dosyaları içe aktarımda kullanıcı **10 dakikalık tek seferlik gözden geçirmeden** geçirir (yanlış/bayat notları siler); sağ çıkan olgular `source_tier=user_asserted` (≤.95) alır — gerekçe: kullanıcının kendi oturumlarında biriktirip fiilen sahiplendiği notlar, gözden geçirme karantinayı kaldıran kullanıcı onayı sayılır. Böylece §5-Hafıza-#1 karantina kuralı bozulmadan W1–2 kabul kriteri (tohumdan `<hafiza>` enjeksiyonu) çalışır.

```
# 2) Repo iskeleti (~/code/kahya)
#   kahyad/            (Go daemon)        — main, launchd plist, goose migrasyon + şema
#   kahyad/cmd/kahya/  (Go CLI)           — tek-atış + REPL + `kahya log --trace <id>`, UDS
#   worker/            (Python)           — claude-agent-sdk harness + MCP istemcisi (sürüm pinli + lock)
#   mcp/memory/        (Go, kahyad içinde)— memory_search/write/forget
#   policy.yaml        (R/W1/W2/W3 + reversible + egress allowlist + gizli-şerit + fs-deny globları)
#   docs/design.html   (tam tasarım artifact'ının kopyası — §9 linki buna güncellenir)
```

```bash
# 3) Yerel modelleri indir (~20GB, resume destekli — ComfyUI ile bellek çekişmesine dikkat)
huggingface-cli download <Qwen3-30B-A3B-4bit> <whisper-large-v3-turbo> <Qwen3-Embedding-0.6B>
say -v '?' | grep -i yelda   # ⚑ Türkçe ses mevcut mu? yoksa Sistem Ayarları > Erişilebilirlik'ten indir
```

⚑ **Sırlar (Keychain) + kod imzalama:** kahyad Makefile'da sabit self-signed kimlikle imzalanır (`codesign -s 'Kahya Dev' kahyad` — Apple Silicon'da Go'nun ad-hoc imzası her build'de değişip Keychain ACL'ini kırar). Sırlar `-T $(which kahyad)` ile eklenir:
```bash
security add-generic-password -s kahya.anthropic -a kahya -T "$(which kahyad)" -w   # ANTHROPIC key
security add-generic-password -s kahya.telegram  -a kahya -T "$(which kahyad)" -w   # BotFather token
security add-generic-password -s kahya.anchor    -a kahya -T "$(which kahyad)" -w   # dış-çapa deploy key
```

⚑ **macOS izin checklist'i** (TCC grant'leri "sorumlu sürece" bağlanır — her W3/W6 aracını **launchd altında** test et, terminalden değil; Automation diyaloglarını **gündüz elle** tetikleyip onayla, gece 03:00 rutini diyaloğu göremez):

| Süreç | İzin |
|---|---|
| Hammerspoon | Accessibility (+ PTT için Mikrofon / Input Monitoring) |
| AppleScript/JXA gönderen worker | hedef-uygulama-başına Automation |
| korunan dizinleri okuyan `fs` aracı | Full Disk Access |

Keychain kilitli/erişilemezse (SSH oturumu, lock-keychain zaman aşımı → `errSecInteractionNotAllowed`): bulut şeridi **fail-fast + kullanıcı bildirimi**, yerel gizli-şerit çalışmaya devam eder.

Sonra ilk uçtan-uca döngüyü kur: **`kahyad` + `kahya` CLI + tek `fs` okuma aracı + `UserPromptSubmit` hafıza kancası**. İlk "hatırladı" anını hafta 1'de yakala — sonraki sekiz haftanın motivasyonu budur.

---

## 8 · Şimdi İNŞA ETME (ertelenenler)

Gömülü NATS · SQLCipher · yerel GPT-OSS-120B · ekran gözlemi firehose · Endpoint Security uzantısı · Virtualization.framework VM · vision computer-use · wake-word · XTTS/Piper · intent-LoRA · iPhone app (Telegram zaten telefon) · hash-zincir tiyatrosu (dış-çapa yeter) · SwiftUI hafıza tarayıcısı (hafıza = markdown, editörle aç) · iki-zamanlı fact/predicate grafiği (v2 katmanı, ancak retrieval ilişkisel sorularda başarısız olursa) · saga telafi-yürütücüsü (kademeli yürütme + idempotency/makbuz — §6 W4 — yeterli) · reranker (yalnız eval precision düşerse) · MLX gizli-şerit süreç-izolasyon Unix-soket proxy'si (yalnız süreç sınırı gerekirse).

---

## 9 · Referans

- **Tam tasarım (diyagram + senaryolar + tablolar):** https://claude.ai/code/artifact/466f3b05-4443-4ba9-ab8c-02ee17ab315f · repoda `docs/design.html`.
- **Anahtar kütüphaneler:** `claude-agent-sdk` (Python, sürüm pinli), MCP, `sqlc` + **`goose`/`golang-migrate`**, `sqlite-vec` (≥0.1.9), `robfig/cron/v3`, `mlx-whisper`, `mlx_lm.server`, Hammerspoon, Telegram bot **`gopkg.in/telebot.v4` (Go — kahyad içinde, WYSIWYE onay kapısının parçası; grammY/TS DEĞİL — iki-süreç yığınıyla çelişir)**.
- **Dosya düzeni:** kod = `~/code/kahya` (git); hafıza = `~/Kahya/memory/*.md` (git, private remote); indeks/defter = `~/Library/Application Support/Kahya/brain.db` (SQLite WAL); yedek = `~/Kahya/backups/brain-YYYYMMDD.db`; sırlar = macOS Keychain (`kahya.anthropic` / `kahya.telegram` / `kahya.anchor`).
- **Yedekleme:** `~/Kahya` git → private remote (gecelik push); `brain.db` gecelik `VACUUM INTO` (son 7 kopya) — defter markdown'dan türetilemez, tek diskte kalamaz.
- **Model ID'leri:** `claude-opus-4-8`, `claude-sonnet-5`, `claude-haiku-4-5`, `claude-fable-5`. Yerel: `whisper-large-v3-turbo`, `Qwen3-Embedding-0.6B`, `Qwen3-30B-A3B`.

**MVP tamamlandı sayılır:** 2 hafta kesintisiz günlük kullanım · sıfır veri-kaybı olayı (yedekten geri-dönüş bir kez tatbik edildi) · ≥10 komut/gün · haftada ≥5 "hatırladı" anı · egress/gizli-şerit/W3 değişmezleri kod-testli (CI'da yeşil).
