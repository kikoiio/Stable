"""Content fingerprints for the dependencies used by Stable's two checks."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re

from erc import kicad_cli_version, kicad_environment
from schematic import CONNECTION_CHECKER_ID, CONNECTION_CHECKER_VERSION, authorized


ERC_FAMILY = 'kicad.erc'
CONNECTION_FAMILY = 'sensor.connection'
_PATH_VARIABLE = re.compile(r'\$\{([A-Za-z_][A-Za-z0-9_]*)\}')
_LIB_ID = re.compile(r'\(lib_id\s+"((?:\\.|[^"\\])+)"')
_TOKEN = re.compile(r'"(?:\\.|[^"\\])*"|[()]|[^\s()]+')


def _sha256(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _canonical(value: object) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(',', ':')).encode('utf-8')


def _source(kind: str, identity: str, *, digest: str = '', state: str = 'present', reason: str = '') -> dict:
    value = {'kind': kind, 'identity': identity, 'digest': digest, 'state': state}
    if reason:
        value['reason'] = reason
    return value


def _read_source(path: Path, kind: str, identity: str, *, required: bool) -> tuple[dict, bytes | None]:
    try:
        data = path.read_bytes()
    except FileNotFoundError:
        if required:
            return _source(kind, identity, state='missing_required', reason=f'required {identity} is missing'), None
        return _source(kind, identity, state='absent_optional'), None
    except OSError as exc:
        return _source(kind, identity, state='unreadable', reason=f'{identity} cannot be read: {exc.strerror or exc.__class__.__name__}'), None
    return _source(kind, identity, digest=_sha256(data)), data


def _erc_settings(data: bytes) -> dict:
    parsed = json.loads(data.decode('utf-8'))

    def relevant(value):
        if isinstance(value, dict):
            selected = {}
            for key, item in value.items():
                nested = relevant(item)
                if 'erc' in key.lower() or nested is not None:
                    selected[key] = item if 'erc' in key.lower() else nested
            return selected or None
        if isinstance(value, list):
            selected = [item for item in (relevant(child) for child in value) if item is not None]
            return selected or None
        return None

    return relevant(parsed) or {}


def _parse_sexpr(text: str):
    tokens = _TOKEN.findall(text)
    index = 0

    def parse_list() -> list:
        nonlocal index
        if index >= len(tokens) or tokens[index] != '(':
            raise ValueError('expected opening parenthesis')
        index += 1
        result = []
        while index < len(tokens) and tokens[index] != ')':
            token = tokens[index]
            if token == '(':
                result.append(parse_list())
            else:
                index += 1
                if token.startswith('"'):
                    result.append(json.loads(token))
                else:
                    result.append(token)
        if index >= len(tokens):
            raise ValueError('unclosed parenthesis')
        index += 1
        return result

    parsed = parse_list()
    if index != len(tokens):
        raise ValueError('trailing S-expression data')
    return parsed


def _library_map(text: str) -> dict[str, str]:
    tree = _parse_sexpr(text)
    if not isinstance(tree, list) or not tree or tree[0] != 'sym_lib_table':
        raise ValueError('symbol library table root must be sym_lib_table')
    libraries = {}
    for item in tree[1:]:
        if not isinstance(item, list) or not item or item[0] != 'lib':
            continue
        fields = {field[0]: field[1] for field in item[1:] if isinstance(field, list) and len(field) > 1}
        if not isinstance(fields.get('name'), str) or not isinstance(fields.get('uri'), str):
            raise ValueError('library entry must include name and uri')
        libraries[fields['name']] = fields['uri']
    return libraries


def _system_variable(name: str) -> str | None:
    value = os.environ.get(name)
    if value:
        return value
    # KiCad's packaged Linux defaults are stable filesystem locations. Use a
    # default only when that installation path exists; otherwise report it as
    # unavailable rather than inventing a location.
    if name == 'KICAD9_SYMBOL_DIR':
        default = Path('/usr/share/kicad/symbols')
        if default.is_dir():
            return str(default)
    return None


def _resolve_uri(uri: str, root: Path, env: dict[str, str], sources: list[dict]) -> Path | None:
    missing = False

    def substitute(match):
        nonlocal missing
        name = match.group(1)
        value = str(root.resolve()) if name == 'KIPRJMOD' else (env.get(name) or _system_variable(name))
        if value is None:
            missing = True
            sources.append(_source('path_variable', name, state='missing_required', reason=f'required path variable {name} is unavailable'))
            return ''
        sources.append(_source('path_variable', name, digest=_sha256(value.encode('utf-8'))))
        return value

    expanded = _PATH_VARIABLE.sub(substitute, uri)
    if missing or _PATH_VARIABLE.search(expanded):
        return None
    path = Path(expanded)
    if not path.is_absolute():
        path = root / path
    return path


def _snapshot(family: str, sources: list[dict], checker_id: str, checker_version: str | None, available: bool, reason: str = '') -> dict:
    sources.sort(key=lambda source: (source['kind'], source['identity']))
    semantic = {
        'schema_version': 1,
        'family': family,
        'sources': [{key: value for key, value in source.items() if key != 'reason'} for source in sources],
        'checker_id': checker_id,
        'checker_version': checker_version or '',
        'available': available,
    }
    return {
        **semantic,
        'fingerprint': _sha256(_canonical(semantic)),
        'reason': reason,
    }


def collect_dependencies(path: Path, root: Path, *, env: dict[str, str] | None = None, checker_version: str | None = None) -> list[dict]:
    """Describe ERC and connection inputs without modifying project files."""
    path = Path(path)
    root = Path(root)
    if not authorized(path, root):
        raise ValueError('design outside allowed root')
    env = dict(env if env is not None else kicad_environment(root))
    env['KIPRJMOD'] = str(root.resolve())
    version = checker_version if checker_version is not None else kicad_cli_version(env)

    erc_sources: list[dict] = []
    unavailable: list[str] = []
    project_path = root / (path.stem + '.kicad_pro')
    project_source, project_data = _read_source(project_path, 'project_erc', 'project ERC settings', required=True)
    if project_data is None:
        unavailable.append(project_source.get('reason', 'project ERC settings unavailable'))
    else:
        try:
            settings = _erc_settings(project_data)
            project_source['digest'] = _sha256(_canonical(settings))
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
            project_source['state'] = 'malformed'
            project_source['reason'] = f'project ERC settings are malformed: {exc}'
            unavailable.append(project_source['reason'])
    erc_sources.append(project_source)

    try:
        schematic_text = path.read_text(encoding='utf-8')
        selected = sorted({item.split(':', 1)[0] for item in _LIB_ID.findall(schematic_text) if ':' in item})
    except (OSError, UnicodeDecodeError) as exc:
        schematic_text = ''
        selected = []
        unavailable.append(f'design cannot be inspected for symbol dependencies: {exc}')

    project_table_path = root / 'sym-lib-table'
    project_table_source, project_table_data = _read_source(project_table_path, 'project_symbol_table', 'project sym-lib-table', required=False)
    project_libraries = {}
    if project_table_data is not None:
        try:
            project_libraries = _library_map(project_table_data.decode('utf-8'))
        except (UnicodeDecodeError, ValueError, json.JSONDecodeError) as exc:
            project_table_source['state'] = 'malformed'
            project_table_source['reason'] = f'project symbol library table is malformed: {exc}'
            unavailable.append(project_table_source['reason'])
    erc_sources.append(project_table_source)

    config_dir = Path(env['XDG_CONFIG_HOME']) / 'kicad' / '9.0'
    global_table_path = config_dir / 'sym-lib-table'
    global_table_source, global_table_data = _read_source(global_table_path, 'global_symbol_table', 'selected global sym-lib-table', required=False)
    global_libraries = {}
    if global_table_data is not None:
        try:
            global_libraries = _library_map(global_table_data.decode('utf-8'))
        except (UnicodeDecodeError, ValueError, json.JSONDecodeError) as exc:
            global_table_source['state'] = 'malformed'
            global_table_source['reason'] = f'global symbol library table is malformed: {exc}'
            unavailable.append(global_table_source['reason'])

    used_global = False
    for nickname in selected:
        uri = project_libraries.get(nickname)
        if uri is None:
            uri = global_libraries.get(nickname)
            used_global = True
        if uri is None:
            missing_reason = f'symbol library {nickname} is not present in selected symbol tables'
            erc_sources.append(_source('symbol_library', nickname, state='missing_required', reason=missing_reason))
            unavailable.append(missing_reason)
            continue
        symbol_path = _resolve_uri(uri, root, env, erc_sources)
        if symbol_path is None:
            unavailable.append(f'symbol library {nickname} path cannot be resolved')
            continue
        library_source, library_data = _read_source(symbol_path, 'symbol_library', nickname, required=True)
        if library_data is not None:
            library_source['identity'] = nickname
        erc_sources.append(library_source)
        if library_data is None:
            unavailable.append(library_source.get('reason', f'symbol library {nickname} unavailable'))

    if used_global:
        if global_table_data is None:
            global_table_source['state'] = 'missing_required'
            global_table_source['reason'] = 'selected global symbol library table is missing'
            unavailable.append(global_table_source['reason'])
        erc_sources.append(global_table_source)
    elif global_table_source['state'] == 'malformed' and not project_libraries:
        # A malformed fallback table matters only when ERC would need it.
        erc_sources.append(global_table_source)

    erc_reason = '; '.join(dict.fromkeys(unavailable))
    erc = _snapshot(ERC_FAMILY, erc_sources, 'kicad-cli-erc', version, not unavailable and bool(version), erc_reason)
    if not version:
        erc['reason'] = '; '.join(filter(None, [erc['reason'], 'kicad-cli version unavailable']))
        erc['available'] = False
        erc['fingerprint'] = _sha256(_canonical({key: erc[key] for key in ('schema_version', 'family', 'sources', 'checker_id', 'checker_version', 'available')}))

    connection_available = bool(CONNECTION_CHECKER_ID and CONNECTION_CHECKER_VERSION)
    connection_reason = '' if connection_available else 'connection checker identity or version unavailable'
    connection = _snapshot(
        CONNECTION_FAMILY,
        [],
        CONNECTION_CHECKER_ID,
        CONNECTION_CHECKER_VERSION,
        connection_available,
        connection_reason,
    )
    return [erc, connection]
