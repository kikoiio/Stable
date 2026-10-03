#!/usr/bin/env bash
# Shared trusted review and acceptance helpers for candidate-producing E2E flows.
# Callers provide project_root, run_root, session_id, goal_id, and status_file.

e2e_chat() {
  local session_root=${E2E_SESSION_ROOT:-$run_root/dev-install/share}
  python3 "$project_root/tests/e2e/chat_client.py" --socket "$run_root/chat.sock" --root "$session_root" "$@"
}

e2e_new_session() {
  local output=$1
  e2e_chat session_create >"$output"
  python3 - "$output" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(m['session']['id'] for m in messages if m.get('type') == 'session'))
PY
}

e2e_allow_pending_approvals() {
  local approval_session=${1:?session ID required} output approval_id
  output="$run_root/approvals-$approval_session.jsonl"
  e2e_chat approval_list --session "$approval_session" >"$output"
  while read -r approval_id; do
    [[ -n "$approval_id" ]] || continue
    e2e_chat approval_resolve --session "$approval_session" --approval "$approval_id" --choice allow_once >/dev/null
  done < <(python3 - "$output" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    msg = json.loads(line)
    for approval in msg.get('approvals', []):
        print(approval['id'])
PY
  )
}

e2e_accept_ready_candidate() {
  local acceptance_session=${1:?session ID required}
  local decision_id=${2:?decision ID required}
  local candidate_id review_file
  candidate_id=$(python3 - "$status_file" <<'PY'
import json, sys
snapshot = json.load(open(sys.argv[1]))['snapshot']
action = next(a for a in snapshot['actions'] if a['status'] in ('candidate_ready', 'awaiting_accept'))
print('candidate-' + action['id'])
PY
)
  review_file="$run_root/review-$decision_id.jsonl"
  e2e_chat review_get --session "$acceptance_session" --candidate "$candidate_id" >"$review_file"
  read -r preview_digest candidate_digest formal_digest < <(python3 - "$review_file" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
review = next(m['review'] for m in messages if m.get('type') == 'review')
findings = review.get('findings', [])
if not findings or any(f.get('result') != 'pass' for f in findings):
    raise SystemExit('normal acceptance requires all candidate findings to pass')
print(review['digest'], review['candidate_digest'], review['formal_digest'])
PY
)
  e2e_chat review_accept --session "$acceptance_session" --candidate "$candidate_id" \
    --decision "$decision_id" --preview-digest "$preview_digest" \
    --candidate-digest "$candidate_digest" --formal-digest "$formal_digest" --mode normal \
    >"$run_root/acceptance-$decision_id.jsonl"
  python3 - "$run_root/acceptance-$decision_id.jsonl" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
assert next(m['receipt']['id'] for m in messages if m.get('type') == 'acceptance')
PY
}
