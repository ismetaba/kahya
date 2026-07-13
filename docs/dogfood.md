# Kâhya — 2 haftalık dogfood takip defteri (W78-06)

> **Kural (HANDOFF §9, aynen):**
> **MVP 2 hafta gerçek günlük kullanımdan sağ çıkmadan v1'den hiçbir şey başlamaz.**

Bu defter, MVP'nin **gerçek günlük kullanımda** 2 hafta (14 gün) kesintisiz
ayakta kaldığını kaydeder. HANDOFF §8'deki hiçbir v2 işi (NATS, SQLCipher,
GPT-OSS-120B, ekran-gözlem firehose, SwiftUI tarayıcı, bitemporal graph,
wake-word, XTTS/Piper, iPhone uygulaması, …) bu 14 gün temiz geçmeden
**başlamaz**.

## Nasıl işler

- Her gün bir satır doldur: o gün verilen komut sayısı, "hatırladı" anı sayısı,
  ve olay sütunu.
- **Olay sütunu makine-okunur.** Bir olay olduysa hücreye `type:` önekiyle
  türü yaz. Geçerli türler: `data-loss` · `safety` · `crash`. `kahya readiness
  --phase=complete` bu sütunu ayrıştırır (`kahyad/internal/readiness.
  ParseDogfoodIncidents`): **herhangi bir data-loss türü satırı** §9
  "sıfır veri-kaybı olayı" kapısını **kırmızıya** düşürür. safety ve crash
  türleri raporlanır ama bu belirli kapıyı düşürmez.
- Olay yoksa hücreye `—` yaz.
- Ayrıştırıcı, aşağıdaki **kod-çiti (```) içindeki örnekleri yok sayar** — yalnız
  gerçek (çitsiz) günlük-takip tablosu satırları işlenir.

### Kabul edilen olay satırı biçimi (örnekler)

Ayrıştırıcı, bir satırın herhangi bir yerinde `type: <tür>` kalıbını arar
(büyük/küçük harf duyarsız; tür küçük harfe indirgenir). Örnekler:

```
| 2026-07-20 | 12 | 2 | type: data-loss episodes tablosu bir restart sonrası boşaldı |
| 2026-07-21 | 14 | 1 | type: crash palet ⌥Space sonrası kahyad paneli çöktü |
| 2026-07-22 | 11 | 3 | type: safety gizli-şerit içeriği için yanlışlıkla bulut çağrısı denendi |
| 2026-07-23 | 15 | 2 | — |
```

## Günlük takip (14 gün)

Pencere başlangıcı: `____-__-__` · bitiş: `____-__-__`

| Gün | Tarih | Komut (≥10?) | Hatırladı anı | Olay (`type:` önekli) |
| --- | --- | --- | --- | --- |
| 1  | ____-__-__ |  |  | — |
| 2  | ____-__-__ |  |  | — |
| 3  | ____-__-__ |  |  | — |
| 4  | ____-__-__ |  |  | — |
| 5  | ____-__-__ |  |  | — |
| 6  | ____-__-__ |  |  | — |
| 7  | ____-__-__ |  |  | — |
| 8  | ____-__-__ |  |  | — |
| 9  | ____-__-__ |  |  | — |
| 10 | ____-__-__ |  |  | — |
| 11 | ____-__-__ |  |  | — |
| 12 | ____-__-__ |  |  | — |
| 13 | ____-__-__ |  |  | — |
| 14 | ____-__-__ |  |  | — |

## Kuzey-yıldızı tallosu (yürüyen)

Bunlar §9 sözleşmesi değil, HANDOFF §6 kuzey-yıldızı **hedefleri**dir —
`kahya readiness` bunları **raporlar** ama çıkış kodunu **belirlemez**.
Otoritatif değerler `kahya metrics` / `kahya readiness --phase=complete`
çıktısındandır; buradaki tablo elle-izleme kolaylığı içindir.

| Metrik | Hedef | Hafta 1 | Hafta 2 |
| --- | --- | --- | --- |
| komut/gün (ortalama) | ≥10 |  |  |
| açıklama-turu olmadan tamamlanan komut oranı | ≥%60 (≤%40 açıklama-turu) |  |  |
| palet-aç→ilk-token p50 | <1.5s |  |  |
| hatırladı/hafta | ≥5 |  |  |

## §9 "MVP tamamlandı" kapıları (sözleşme)

`kahya readiness --phase=complete` şunların **hepsi** yeşilse 0 döner:

- **2 hafta kesintisiz günlük kullanım** — ≥14 gün kayıtlı komut etkinliği
  (`window_ok`).
- **≥10 komut/gün** — pencere ortalaması ≥10 **ve** ≥14 ayrı günün her biri ≥10
  (sürdürülebilir okuma; `commands_per_day_ok`).
- **haftada ≥5 "hatırladı" anı** — pencere başına haftalık ortalama ≥5
  (`remembered_ok`).
- **sıfır veri-kaybı olayı** — yukarıdaki olay sütununda hiçbir data-loss türü
  satırı yok (`data_loss_ok`); yedekten geri-dönüş bir kez tatbik edilmiş
  (W78-05 `restore.drill.result`, `--phase=start` inşa kapısı).
- **egress/gizli-şerit/W3 değişmezleri kod-testli, CI'da yeşil** — `make test` +
  `make invariants` (W78-03), `make readiness` orkestrasyonunda koşar.

Tam eşleme için bkz. [`docs/mvp-readiness.md`](./mvp-readiness.md).
