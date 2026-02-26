#!/usr/bin/env python3
"""Parse Claude Code transcript JSONL into CSV.

Usage:
    python3 parse_transcript.py <transcript.jsonl> [count] [--csv output.csv]

    count: number of meaningful entries to show (default: all)
    --csv: output to CSV file (default: print to terminal as table)
"""
import json, sys, csv, io
from datetime import datetime

path = sys.argv[1]
count = None
csv_path = None

i = 2
while i < len(sys.argv):
    if sys.argv[i] == '--csv' and i + 1 < len(sys.argv):
        csv_path = sys.argv[i + 1]
        i += 2
    else:
        try:
            count = int(sys.argv[i])
        except:
            pass
        i += 1

# Read entire file (for CSV we want everything, for terminal we limit)
with open(path, 'rb') as f:
    if count and not csv_path:
        # For terminal, read tail only
        f.seek(0, 2)
        size = f.tell()
        tail = 1_000_000  # 1MB
        offset = max(0, size - tail)
        f.seek(offset)
        data = f.read().decode('utf-8', errors='replace')
        lines = data.strip().split('\n')
        if offset > 0:
            lines = lines[1:]
    else:
        data = f.read().decode('utf-8', errors='replace')
        lines = data.strip().split('\n')

def fmt_ts(obj):
    ts = obj.get('timestamp')
    if not ts:
        return ''
    if isinstance(ts, str):
        try:
            dt = datetime.fromisoformat(ts.replace('Z', '+00:00'))
            # Convert to local time
            dt = dt.astimezone()
            return dt.strftime('%Y-%m-%d %H:%M:%S')
        except:
            return ts[:19]
    try:
        dt = datetime.fromtimestamp(ts / 1000 if ts > 1e12 else ts)
        return dt.strftime('%Y-%m-%d %H:%M:%S')
    except:
        return ''

def short_ts(obj):
    ts = obj.get('timestamp')
    if ts:
        try:
            dt = datetime.fromtimestamp(ts / 1000 if ts > 1e12 else ts)
            return dt.strftime('%H:%M:%S')
        except:
            pass
    return ''

rows = []
for line in lines:
    try:
        obj = json.loads(line)
    except:
        continue

    t = obj.get('type', '')
    rid = obj.get('requestId', '')
    uuid = obj.get('uuid', '')
    parent_uuid = obj.get('parentUuid', '')
    is_err = obj.get('isApiErrorMessage', False)
    is_sidechain = obj.get('isSidechain', False)
    timestamp = fmt_ts(obj)
    session_id = obj.get('sessionId', '')
    cwd = obj.get('cwd', '')
    version = obj.get('version', '')
    git_branch = obj.get('gitBranch', '')
    slug = obj.get('slug', '')
    user_type = obj.get('userType', '')

    base = {
        'time': timestamp, 'type': t, 'uuid': uuid, 'parentUuid': parent_uuid,
        'requestId': rid, 'sessionId': session_id, 'isApiErrorMessage': is_err,
        'isSidechain': is_sidechain, 'userType': user_type,
        'cwd': cwd, 'version': version, 'gitBranch': git_branch, 'slug': slug,
        'message_role': '', 'content_type': '', 'tool_name': '', 'tool_use_id': '',
        'is_error': '', 'content': ''
    }

    if t == 'assistant':
        msg = obj.get('message', {})
        role = msg.get('role', '')
        content = msg.get('content', [])
        if role != 'assistant':
            row = {**base, 'message_role': role}
            rows.append(row)
            continue
        if not content:
            row = {**base, 'message_role': role}
            rows.append(row)
            continue
        for b in content:
            bt = b.get('type', '')
            row = {**base, 'message_role': role, 'content_type': bt}
            if bt == 'text':
                row['content'] = b.get('text', '')[:2000]
            elif bt == 'tool_use':
                row['tool_name'] = b.get('name', '')
                row['tool_use_id'] = b.get('id', '')
                inp = b.get('input', {})
                row['content'] = json.dumps(inp, ensure_ascii=False)[:2000]
            elif bt == 'thinking':
                row['content'] = b.get('thinking', '')[:500]
            else:
                row['content'] = json.dumps(b, ensure_ascii=False)[:500]
            rows.append(row)

    elif t == 'human':
        msg = obj.get('message', {})
        role = msg.get('role', '')
        content = msg.get('content', [])
        if not content:
            row = {**base, 'message_role': role}
            rows.append(row)
            continue
        for b in content:
            if isinstance(b, str):
                row = {**base, 'message_role': role, 'content_type': 'text', 'content': b[:2000]}
                rows.append(row)
            elif isinstance(b, dict):
                bt = b.get('type', '')
                row = {**base, 'message_role': role, 'content_type': bt}
                if bt == 'text':
                    row['content'] = b.get('text', '')[:2000]
                elif bt == 'tool_result':
                    row['tool_use_id'] = b.get('tool_use_id', '')
                    row['is_error'] = b.get('is_error', False)
                    cont = b.get('content', '')
                    if isinstance(cont, list):
                        row['content'] = json.dumps(cont, ensure_ascii=False)[:2000]
                    else:
                        row['content'] = str(cont)[:2000]
                else:
                    row['content'] = json.dumps(b, ensure_ascii=False)[:500]
                rows.append(row)

    else:
        # progress, system, user, file-history-snapshot, queue-operation, etc.
        data = obj.get('data', '')
        tool_use_id = obj.get('toolUseID', '')
        row = {**base, 'tool_use_id': tool_use_id}
        if data:
            row['content'] = json.dumps(data, ensure_ascii=False)[:500] if not isinstance(data, str) else data[:500]
        rows.append(row)

if count:
    rows = rows[-count:]

fields = ['time', 'type', 'uuid', 'parentUuid', 'requestId', 'sessionId',
          'isApiErrorMessage', 'isSidechain', 'userType', 'cwd', 'version',
          'gitBranch', 'slug', 'message_role', 'content_type', 'tool_name',
          'tool_use_id', 'is_error', 'content']

if csv_path:
    with open(csv_path, 'w', newline='', encoding='utf-8') as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        w.writerows(rows)
    print(f'Wrote {len(rows)} rows to {csv_path}')
else:
    # Terminal: compact table with key columns only
    for r in rows:
        r['content'] = r['content'].replace('\n', ' | ')[:100] if r.get('content') else ''
        r['requestId'] = r['requestId'][:16]
        r['time'] = r['time'][11:] if len(r['time']) > 11 else r['time']

    print(f'{"TIME":<9} {"TYPE":<12} {"ROLE":<10} {"C_TYPE":<12} {"TOOL":<10} {"RID":<18} CONTENT')
    print('-' * 130)
    for r in rows:
        print(f'{r["time"]:<9} {r["type"]:<12} {r["message_role"]:<10} {r["content_type"]:<12} {r["tool_name"]:<10} {r["requestId"]:<18} {r["content"]}')
