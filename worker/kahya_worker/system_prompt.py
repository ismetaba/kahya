"""The frozen Turkish system-prompt prefix (HANDOFF §4 ⚑ cost governor:
"İstem önbelleği: donmuş sistem-öneki + araç tanımları, 1-saat TTL").

`SYSTEM_PROMPT` below, together with `kahya_worker.__main__.ALLOWED_TOOLS`,
form the static prefix Anthropic's prompt cache keys on. Both must stay
BYTE-IDENTICAL across every task and every future worker change - editing
either one invalidates the cache prefix and multiplies API cost 5-10x per
HANDOFF's own cost-governor note ("sessiz cache-bozan maliyeti 5-10x
katlar"). Treat any change to this constant as a deliberate, reviewed
cache-invalidation event, never an incidental edit.

Note on cache TTL configuration: the pinned `claude-agent-sdk==0.2.111`
(`worker/requirements.lock`) exposes exactly one beta flag on
`ClaudeAgentOptions.betas` - `"context-1m-2025-08-07"` (1M-token context
window) - and no 1-hour-cache-TTL option. Per this task's own instruction
("if the pinned SDK exposes cache-TTL configuration, pin it"), there is
nothing to pin: the SDK does not expose that knob at this pinned version.
This is a deliberate no-op, not an oversight - re-check this comment if
`requirements.lock` ever bumps `claude-agent-sdk`.
"""

SYSTEM_PROMPT = (
    "Sen Kâhya'sın: kullanıcının yerel-öncelikli, gizliliğe duyarlı "
    "kişisel yapay zekâ asistanısın. Her zaman Türkçe konuşur, kısa ve "
    "net yanıtlar verirsin. Sana bir <hafiza> bloğu verilirse, içindeki "
    "bilgi kullanıcı hakkında daha önce kaydedilmiş gerçek geçmiş "
    "bağlamdır; bu bilgiyi doğal bir şekilde yanıtına yedir, blok "
    "biçimini veya etiketlerini kullanıcıya asla gösterme. Emin "
    "olmadığın konularda tahmin yürütmek yerine açıkça sorarsın. Para, "
    "sağlık ve kimlik gibi hassas konularda özellikle dikkatli, ölçülü "
    "ve gizliliğe saygılı davranırsın."
)
