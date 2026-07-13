#!/usr/bin/env bash
#
# kahya-dev-env.sh — provision the W78-02 isolated dev profile (KAHYA_ENV=dev)
# for the red-team eval. Idempotent; it NEVER touches the production profile
# (~/Kahya, ~/Library/Application Support/Kahya, the com.kahya.kahyad launchd
# label) — only the "-dev"-suffixed dev tree.
#
# What it does:
#   1. creates ~/Kahya-dev/memory as a git repo (+ ~/Kahya-dev/backups)
#   2. creates ~/Library/Application Support/Kahya-dev/ and migrates the dev
#      brain.db there by booting bin/kahyad once under KAHYA_ENV=dev (the SAME
#      store.Open migration path the daemon uses) and stopping it
#   3. renders dev/launchd/com.kahya.dev.plist into ~/Library/LaunchAgents/
#      (label com.kahya.dev, dev socket) — but does NOT launchctl load it
#
# It deliberately does NOT load/start any launchd agent and does NOT touch the
# production daemon: installing/loading the dev agent and running a live drill
# are left to the operator (see the printed instructions at the end).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

DEV_DATA_DIR="${HOME}/Library/Application Support/Kahya-dev"
DEV_LOG_DIR="${DEV_DATA_DIR}/logs"
DEV_DB="${DEV_DATA_DIR}/brain.db"
DEV_SOCK="${DEV_DATA_DIR}/kahyad-dev.sock"
DEV_MEM_DIR="${HOME}/Kahya-dev/memory"
DEV_BACKUP_DIR="${HOME}/Kahya-dev/backups"
DEV_POLICY="${REPO_ROOT}/policy.dev.yaml"
LAUNCH_AGENTS_DIR="${HOME}/Library/LaunchAgents"
PLIST_SRC="${REPO_ROOT}/dev/launchd/com.kahya.dev.plist"
PLIST_DST="${LAUNCH_AGENTS_DIR}/com.kahya.dev.plist"

# --- hard guard: never operate on the production profile ---
PROD_DATA_DIR="${HOME}/Library/Application Support/Kahya"
PROD_MEM_DIR="${HOME}/Kahya"
case "${DEV_DATA_DIR}" in
	"${PROD_DATA_DIR}"|"${PROD_MEM_DIR}")
		echo "FATAL: dev data dir resolved to the production path — refusing." >&2
		exit 1
		;;
esac

echo "==> provisioning the Kahya dev profile (KAHYA_ENV=dev)"
echo "    data:   ${DEV_DATA_DIR}"
echo "    memory: ${DEV_MEM_DIR}"
echo "    socket: ${DEV_SOCK}"
echo "    policy: ${DEV_POLICY} (deny-all egress)"

# 1. dev memory git repo + backups (idempotent).
mkdir -p "${DEV_MEM_DIR}" "${DEV_BACKUP_DIR}" "${DEV_DATA_DIR}" "${DEV_LOG_DIR}"
if [ ! -d "${DEV_MEM_DIR}/.git" ]; then
	git -C "${DEV_MEM_DIR}" init -q
	git -C "${DEV_MEM_DIR}" config user.email "kahya-dev@example.invalid"
	git -C "${DEV_MEM_DIR}" config user.name "Kahya Dev Profile"
	echo "    created dev memory git repo"
else
	echo "    dev memory git repo already present"
fi

# 2. migrate the dev brain.db via the daemon's own store.Open path.
if [ ! -f "${DEV_POLICY}" ]; then
	echo "FATAL: ${DEV_POLICY} not found (expected committed deny-all dev policy)." >&2
	exit 1
fi
KAHYAD_BIN="${REPO_ROOT}/bin/kahyad"
if [ ! -x "${KAHYAD_BIN}" ]; then
	echo "    NOTE: ${KAHYAD_BIN} not built — run 'make build' first, then re-run this script"
	echo "          to migrate the dev brain.db. Skipping db migration for now."
else
	if [ -f "${DEV_DB}" ]; then
		echo "    dev brain.db already present (migrations are idempotent; re-running to catch new ones)"
	fi
	echo "    migrating dev brain.db via a short-lived KAHYA_ENV=dev kahyad boot"
	rm -f "${DEV_SOCK}"
	KAHYA_ENV=dev KAHYA_POLICY_PATH="${DEV_POLICY}" "${KAHYAD_BIN}" \
		>"${DEV_LOG_DIR}/provision.out.log" 2>"${DEV_LOG_DIR}/provision.err.log" &
	KAHYAD_PID=$!
	# Wait for the dev socket to appear (migrations run at boot, before the
	# UDS listener accepts). Then stop the short-lived daemon.
	for _ in $(seq 1 60); do
		if [ -S "${DEV_SOCK}" ]; then break; fi
		if ! kill -0 "${KAHYAD_PID}" 2>/dev/null; then
			echo "FATAL: dev kahyad exited during provisioning — see ${DEV_LOG_DIR}/provision.err.log" >&2
			exit 1
		fi
		sleep 0.25
	done
	kill "${KAHYAD_PID}" 2>/dev/null || true
	wait "${KAHYAD_PID}" 2>/dev/null || true
	if [ -f "${DEV_DB}" ]; then
		echo "    dev brain.db migrated: ${DEV_DB}"
	else
		echo "FATAL: dev brain.db was not created — see ${DEV_LOG_DIR}/provision.err.log" >&2
		exit 1
	fi
fi

# 3. render (but do NOT load) the dev launchd plist.
mkdir -p "${LAUNCH_AGENTS_DIR}"
sed -e "s#__KAHYA_REPO_ROOT__#${REPO_ROOT}#g" \
	-e "s#__KAHYA_DEV_LOG_DIR__#${DEV_LOG_DIR}#g" \
	"${PLIST_SRC}" > "${PLIST_DST}"
echo "    rendered dev launchd plist: ${PLIST_DST} (label com.kahya.dev — NOT loaded)"

cat <<EOF

==> dev profile ready.

To run the red-team eval (hermetic, no network — the four scenarios also run
under 'make test'):

    KAHYA_ENV=dev ./bin/kahya eval redteam        # or: make eval-redteam

To load the dev launchd agent yourself (optional, user-assist):

    launchctl bootstrap gui/\$(id -u) "${PLIST_DST}"
    launchctl kickstart -k gui/\$(id -u)/com.kahya.dev
    # unload with: launchctl bootout gui/\$(id -u)/com.kahya.dev

The production profile (~/Kahya, com.kahya.kahyad) was not touched.
EOF
