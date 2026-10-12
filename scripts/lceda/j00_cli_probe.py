#!/usr/bin/env python3
"""Exercise LCEDA Pro's documented local CLI/API against a generated project.

The probe only creates a disposable project when the client reports a half- or
fully-offline mode. It then copies that project to a candidate directory,
modifies one resistor, saves, closes, reopens in a fresh session, verifies the
saved value, runs schematic DRC and exports a BOM through the official API.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
from typing import Any

try:
    from .j00_isolation import ProcessSupervisor, sha256_tree
except ImportError:  # pragma: no cover - direct script execution
    from j00_isolation import ProcessSupervisor, sha256_tree


class ProbeFailure(RuntimeError):
    pass


def _safe(text: str) -> str:
    run_dir = os.environ.get("J00_RUN_DIR")
    if run_dir:
        text = text.replace(str(Path(run_dir).resolve()), "<run-dir>")
    return text[:1200]


def _activation_required() -> bool:
    """Detect the client's registration redirect after an open failure."""

    home_root = Path(os.environ.get("J00_HOME", ""))
    if not home_root.is_dir():
        return False
    log_files = sorted(
        home_root.glob("**/LCEDA-Pro/logs/**/*.log"),
        key=lambda path: path.stat().st_mtime_ns if path.exists() else 0,
        reverse=True,
    )
    for path in log_files[:4]:
        try:
            text = path.read_text(encoding="utf-8", errors="replace")[-200_000:]
        except OSError:
            continue
        if re.search(r"entry=regist|read-license-file-error|CLIENT_LICENSE", text, re.IGNORECASE):
            return True
    return False


class Client:
    def __init__(self, executable: Path, env: dict[str, str]):
        self.executable = executable
        self.env = env
        # The archive is extracted into a user-owned runner directory, so its
        # chrome-sandbox helper cannot have the root ownership/mode Electron
        # expects. These switches also keep the probe off the runner GPU.
        self.launch_args = [str(executable)]
        self.electron_args = ["--no-sandbox", "--disable-gpu", "--ozone-platform=x11"]
        self.supervisor = ProcessSupervisor(default_timeout=120, env=env)

    def command(self, *args: str, timeout: int = 120) -> dict[str, Any]:
        result = self.supervisor.run(
            [*self.launch_args, *map(str, args), *self.electron_args],
            cwd=self.executable.parent,
            timeout=timeout,
        )
        if not result.ok:
            raise ProbeFailure(
                f"command {args[0] if args else 'cli'} failed "
                f"(exit={result.returncode}, timeout={result.timed_out}): "
                f"{_safe(result.stderr or result.stdout)}"
            )
        try:
            value = json.loads(result.stdout)
        except json.JSONDecodeError as exc:
            raise ProbeFailure(f"command returned non-JSON output: {_safe(result.stdout)}") from exc
        if not isinstance(value, dict) or value.get("ok") is not True:
            raise ProbeFailure(f"command returned an unsuccessful result: {_safe(json.dumps(value))}")
        return value

    def invoke(self, session: str, code: str, *, timeout: int = 120) -> Any:
        payload = self.command(
            "invoke", "--session", session, "--ext-uuid", "eda",
            "--code", code, "--timeout", str(timeout * 1000), timeout=timeout + 20,
        )
        return payload.get("value")


def _session(result: dict[str, Any]) -> str:
    value = result.get("value")
    session = value.get("sessionId") if isinstance(value, dict) else None
    if not isinstance(session, str) or not session:
        raise ProbeFailure("CLI open response did not include a sessionId")
    return session


def _open_session(client: Client, *args: str, timeout: int = 90) -> dict[str, Any]:
    """Open a render session, allowing the packaged bridge to finish booting."""

    last_error: ProbeFailure | None = None
    for attempt in range(3):
        try:
            return client.command("open", *args, "--headless", "true", timeout=timeout)
        except ProbeFailure as error:
            last_error = error
            if _activation_required():
                raise ProbeFailure(
                    "CLIENT_ACTIVATION_REQUIRED: official client redirected /editor "
                    "to entry=regist; provide an authorized activation file before the "
                    "real project round-trip can run"
                ) from error
            if not any(code in str(error) for code in ("BRIDGE_NOT_READY", "REQUEST_TIMEOUT")) or attempt == 2:
                raise
            time.sleep(5 * (attempt + 1))
    assert last_error is not None
    raise last_error


def _write_report(report: dict[str, Any]) -> None:
    artifact_dir = Path(os.environ["J00_ARTIFACT_DIR"])
    artifact_dir.mkdir(parents=True, exist_ok=True)
    # Preserve the packaged client's own startup evidence. The workflow
    # sanitizer replaces the isolated run path and credential-like markers
    # before upload, while this log is the only useful signal when a hidden
    # editor render never exposes the CLI command space.
    home_root = Path(os.environ.get("J00_HOME", ""))
    log_roots = sorted(
        path for path in home_root.glob("**/LCEDA-Pro/logs") if path.is_dir()
    ) if home_root.is_dir() else []
    (artifact_dir / "client-log-roots.txt").write_text(
        "\n".join(str(path) for path in log_roots) + "\n", encoding="utf-8"
    )
    for index, log_root in enumerate(log_roots):
        for source in log_root.rglob("*.log"):
            try:
                relative = source.relative_to(log_root)
                shutil.copy2(source, artifact_dir / "client-logs" / str(index) / relative)
            except OSError:
                continue
    (artifact_dir / "cli-probe.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    with (artifact_dir / "environment.txt").open("a", encoding="utf-8") as stream:
        stream.write(f"official_cli_probe_status={report.get('status', 'failed')}\n")


def _run() -> dict[str, Any]:
    run_dir = Path(os.environ["J00_RUN_DIR"]).resolve()
    client_dir = Path(os.environ["J00_CLIENT_INSTALL_DIR"]).resolve()
    runtime_dir = Path(os.environ["J00_RUNTIME_DIR"]).resolve()
    home = Path(os.environ["J00_HOME"]).resolve()
    executable = client_dir / "lceda-pro" / "lceda-pro"
    if not executable.is_file():
        raise ProbeFailure(f"Linux CLI executable is missing: {executable}")
    executable.chmod(executable.stat().st_mode | 0o111)
    crashpad = executable.with_name("chrome_crashpad_handler")
    if crashpad.is_file():
        # ZIP extraction does not reliably preserve executable bits. Electron
        # starts crashpad before dispatching the first CLI command.
        crashpad.chmod(crashpad.stat().st_mode | 0o111)
    env = os.environ.copy()
    env.update({
        "HOME": str(home),
        "USERPROFILE": str(home),
        "TMPDIR": str(runtime_dir),
        "TMP": str(runtime_dir),
        "TEMP": str(runtime_dir),
        "XDG_CONFIG_HOME": str(Path(os.environ["J00_XDG_CONFIG_HOME"])),
        "XDG_CACHE_HOME": str(Path(os.environ["J00_XDG_CACHE_HOME"])),
        "XDG_DATA_HOME": str(Path(os.environ["J00_XDG_DATA_HOME"])),
        "WAYLAND_DISPLAY": "",
        "LIBGL_ALWAYS_SOFTWARE": "1",
    })
    for key in ("HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"):
        Path(env[key]).mkdir(parents=True, exist_ok=True)

    # On Linux Electron resolves app.getPath("documents") to HOME, so the
    # packaged client reads this file from $HOME/LCEDA-Pro/config.json (not
    # $HOME/Documents/LCEDA-Pro). Keep the full config shape the client writes
    # itself; a minimal object can leave startup in the first-run path and the
    # CLI render never exposes its command space.
    projects_root = home / "LCEDA-Pro"
    config = {
        "type": "OFFLINE",
        "devTools": "",
        "logLevel": "trace",
        "logEnable": True,
        "crashUploadToServer": False,
        "gpu": "disabled",
        "canvas2d": True,
        "sqlTimeout": 2000,
        "connectTimeout": 5000,
        "proxy": {
            "enable": False,
            "protocol": "http",
            "host": "0.0.0.0",
            "port": 0,
            "username": "",
            "password": "",
        },
        "firstRun": False,
        "systemLibName": "lceda-std",
        "APP_PROJECT_DIR": [str(projects_root / "projects")],
    }
    for config_root in (projects_root, home / "Documents" / "LCEDA-Pro"):
        config_root.mkdir(parents=True, exist_ok=True)
        (config_root / "config.json").write_text(
            json.dumps(config, ensure_ascii=False) + "\n", encoding="utf-8"
        )

    client = Client(executable, env)
    report: dict[str, Any] = {
        "schema_version": "j00-cli-probe-v1",
        "status": "failed",
        "stages": {},
        "generated_project": False,
        "candidate_loop": "not_run",
    }
    active_session: str | None = None
    server_started = False
    formal: Path | None = None
    formal_modes: dict[str, int] = {}
    try:
        help_result = client.supervisor.run(
            [*client.launch_args, "--help", *client.electron_args],
            cwd=executable.parent,
            timeout=15,
        )
        help_text = help_result.stdout + "\n" + help_result.stderr
        help_ok = help_result.ok and "commands:" in help_text.lower()
        report["stages"]["help"] = {
            "status": "passed" if help_ok else "limited",
            "exit_code": help_result.returncode,
            "timed_out": help_result.timed_out,
            "preview": _safe(help_text.strip())[:300],
        }

        doctor = client.command("doctor")
        doctor_value = doctor.get("value", {})
        report["stages"]["doctor"] = {
            "status": "passed",
            "connected_before_open": bool(doctor_value.get("connected")),
        }

        # Mark the owner as started before opening so a failed first render is
        # still followed by the CLI stop command in the finally block.
        server_started = True
        opened = _open_session(client)
        active_session = _session(opened)
        report["stages"]["open_headless"] = "passed"

        environment = client.invoke(
            active_session,
            "return {isClient: eda.sys_Environment.isClient(), "
            "isOffline: eda.sys_Environment.isOfflineMode(), "
            "isHalfOffline: eda.sys_Environment.isHalfOfflineMode(), "
            "version: eda.sys_Environment.getEditorCurrentVersion()};",
        )
        if not isinstance(environment, dict):
            raise ProbeFailure("environment API did not return an object")
        report["client"] = {
            "is_client": environment.get("isClient"),
            "is_offline": environment.get("isOffline"),
            "is_half_offline": environment.get("isHalfOffline"),
            "version": environment.get("version"),
        }
        report["stages"]["environment_api"] = "passed"

        project_api = client.command("doc", "api", "--class-name", "DMT_Project")
        methods = project_api.get("value", {}).get("methods", [])
        method_names = [item.get("name") for item in methods if isinstance(item, dict)]
        report["stages"]["project_api_docs"] = {
            "status": "passed" if "createProject" in method_names else "limited",
            "create_project_documented": "createProject" in method_names,
        }

        # Creating a project while online can create a remote object. Stay
        # strictly local: only proceed when the isolated client says offline.
        offline = bool(environment.get("isOffline") or environment.get("isHalfOffline"))
        if not offline:
            report["status"] = "limited"
            report["candidate_loop"] = "skipped_client_not_offline"
            report["reason"] = "isolated client did not report offline/half-offline mode"
            return report

        token = re.sub(r"[^A-Za-z0-9-]", "", os.environ.get("J00_RUN_ID", "run"))[-24:]
        project_name = f"j00-probe-{token.lower()}"
        create_code = (
            "const uuid = await eda.dmt_Project.createProject("
            f"'J00 CLI Probe', '{project_name}', undefined, undefined, "
            "'Disposable offline J00 compatibility fixture', undefined, "
            "EDMT_ProjectFileFormat.EPRJ3); "
            "if (!uuid) throw new Error('createProject returned no project UUID'); "
            "await eda.dmt_Project.openProject(uuid); "
            "const schematicUuid = await eda.dmt_Schematic.createSchematic(); "
            "if (!schematicUuid) throw new Error('createSchematic returned no UUID'); "
            "let info, page; "
            "for (let i = 0; i < 30; i++) { "
            "info = await eda.dmt_Schematic.getSchematicInfo(schematicUuid); "
            "page = info && info.page && info.page[0]; "
            "if (page && page.uuid) break; "
            "await new Promise((r) => setTimeout(r, 1000)); "
            "} "
            "if (!info || !page || !page.uuid) throw new Error('created schematic page did not become ready'); "
            "return {uuid, board: null, schematic: info.name, "
            "page: page.uuid};"
        )
        created = client.invoke(active_session, create_code, timeout=90)
        if not isinstance(created, dict) or not created.get("page"):
            raise ProbeFailure("createProject did not return project/page identity")
        report["generated_project"] = True
        report["stages"]["create_project"] = "passed"

        # The offline client uses the display name for the on-disk project
        # folder/file and may ignore the slug argument above. The isolated
        # projects root starts empty, so a unique directory is the generated
        # project regardless of the name the client chose.
        project_entries = list(projects_root.rglob("*.eprj3")) if projects_root.exists() else []
        matches = sorted({
            path if path.is_dir() else path.parent
            for path in project_entries
        })
        if len(matches) != 1:
            raise ProbeFailure(f"expected one locally generated .eprj3 project; found {len(matches)}")
        formal = matches[0]
        report["project"] = {"format": "eprj3", "file_count": sum(p.is_file() for p in formal.rglob("*"))}

        # Add a known library component using the documented API and capture
        # its real primitive ID for a guarded value change.
        add_code = (
            f"await eda.dmt_EditorControl.openDocument('{created['page']}'); "
            "await new Promise((r) => setTimeout(r, 2500)); "
            "const items = await eda.lib_Device.search('0603WAF1001T5E'); "
            "if (!items || !items.length) throw new Error('offline system library resistor not found'); "
            "const item = items[0]; "
            "const c = await eda.sch_PrimitiveComponent.create(item, 400, 610, undefined, 90, false, true, true); "
            "if (!c) throw new Error('component create returned no primitive'); "
            "const id = c.getState_PrimitiveId(); "
            "const pre = c.getState_OtherProperty(); "
            "if (!pre) throw new Error('created resistor has no properties'); "
            "await eda.sch_PrimitiveComponent.modify(id, {designator: 'R1', otherProperty: pre}); "
            "const pins = await eda.sch_PrimitiveComponent.getAllPinsByPrimitiveId(id); "
            "await eda.sch_Document.save(); "
            "return {id, value: pre.Value, pinCount: pins.length};"
        )
        component = client.invoke(active_session, add_code, timeout=150)
        if not isinstance(component, dict) or not component.get("id"):
            raise ProbeFailure("component creation did not return a primitive ID")
        report["component"] = {
            "reference": "R1",
            "initial_value": component.get("value"),
            "pin_count": component.get("pinCount"),
        }
        report["stages"]["create_and_save_fixture"] = "passed"

        # Close the writer, make a byte-independent candidate, and lock the
        # source tree before opening the candidate in a new editor generation.
        client.command("session", "close", "--session", active_session, "--destroy")
        active_session = None
        time.sleep(1)
        candidate = run_dir / "work" / "candidate.eprj3"
        shutil.copytree(formal, candidate)
        source_digest = sha256_tree(formal)
        for path in [formal, *formal.rglob("*")]:
            if not path.is_symlink():
                formal_modes[path.relative_to(formal).as_posix() if path != formal else "."] = path.stat().st_mode & 0o777
                path.chmod((path.stat().st_mode & 0o777 & ~0o222) | (0o555 if path.is_dir() else 0o444))

        opened = _open_session(client, "--path", str(candidate))
        active_session = _session(opened)
        edit_code = (
            f"await eda.dmt_EditorControl.openDocument('{created['page']}'); "
            "await new Promise((r) => setTimeout(r, 2000)); "
            f"const id = '{component['id']}'; "
            "let item; for (let i = 0; i < 20; i++) { "
            "item = await eda.sch_PrimitiveComponent.get(id); "
            "if (item) break; await new Promise((r) => setTimeout(r, 1000)); } "
            "if (!item) throw new Error('candidate component did not become readable'); "
            "const before = item.getState_OtherProperty(); "
            "const expected = before.Value; "
            "if (expected !== __CLI__.args.expectedValue) throw new Error('expected old value mismatch'); "
            "const after = {...before, Value: __CLI__.args.newValue}; "
            "await eda.sch_PrimitiveComponent.modify(id, {otherProperty: after}); "
            "await eda.sch_Document.save(); "
            "await new Promise((r) => setTimeout(r, 1000)); "
            "return {oldValue: expected, newValue: (await eda.sch_PrimitiveComponent.get(id)).getState_OtherProperty().Value};"
        )
        edit_payload = client.command(
            "invoke", "--session", active_session, "--ext-uuid", "eda",
            "--code", edit_code, "--args", json.dumps({
                "expectedValue": component.get("value"), "newValue": "12kΩ"
            }, ensure_ascii=False), "--timeout", "150000", timeout=170,
        )
        edit_value = edit_payload.get("value")
        if not isinstance(edit_value, dict) or edit_value.get("newValue") != "12kΩ":
            raise ProbeFailure("candidate parameter change did not read back as 12kΩ")
        report["stages"]["candidate_modify_save"] = "passed"
        client.command("session", "close", "--session", active_session, "--destroy")
        active_session = None

        # A new session must read the saved value. API calls are read-only here.
        reopened = _open_session(client, "--path", str(candidate))
        active_session = _session(reopened)
        verify_code = (
            f"await eda.dmt_EditorControl.openDocument('{created['page']}'); "
            "await new Promise((r) => setTimeout(r, 2500)); "
            f"let item; for (let i = 0; i < 20; i++) {{ "
            f"item = await eda.sch_PrimitiveComponent.get('{component['id']}'); "
            "if (item) break; await new Promise((r) => setTimeout(r, 1000)); } "
            "if (!item) throw new Error('reopened component did not become readable'); "
            "return item.getState_OtherProperty().Value;"
        )
        persisted_value = client.invoke(active_session, verify_code)
        if persisted_value != "12kΩ":
            raise ProbeFailure(f"fresh-session value mismatch: {persisted_value!r}")
        report["stages"]["fresh_session_reopen"] = "passed"

        drc = client.invoke(
            active_session,
            "await eda.dmt_EditorControl.openDocument(" + repr(created["page"]) + "); "
            "await new Promise((r) => setTimeout(r, 8500)); "
            "const rows = await eda.sch_Drc.check(true, false, true); "
            "return {violationCount: rows.length, rules: rows.map((x) => x.rule)};",
            timeout=90,
        )
        report["stages"]["schematic_check"] = {"status": "passed", "result": drc}

        bom = client.invoke(
            active_session,
            "const f = await eda.sch_ManufactureData.getBomFile('J00-BOM', 'csv'); "
            "return {name: f.name, size: f.size, text: await f.text()};",
            timeout=90,
        )
        if not isinstance(bom, dict) or int(bom.get("size", 0)) <= 0 or "12kΩ" not in str(bom.get("text", "")):
            raise ProbeFailure("BOM export is empty or does not contain the saved 12kΩ value")
        export_path = run_dir / "artifacts" / "J00-BOM.csv"
        export_path.write_text(str(bom["text"]), encoding="utf-8")
        report["artifacts"] = [{"kind": "bom_csv", "name": "J00-BOM.csv", "size_bytes": export_path.stat().st_size}]
        report["stages"]["bom_export"] = "passed"

        client.command("session", "close", "--session", active_session, "--destroy")
        active_session = None
        candidate_digest = sha256_tree(candidate)
        formal_digest = sha256_tree(formal)
        report["isolation"] = {
            "formal_unchanged": source_digest == formal_digest,
            "source_sha256_before": source_digest,
            "source_sha256_after": formal_digest,
            "candidate_sha256_after": candidate_digest,
            "candidate_path_is_separate": candidate.resolve() != formal.resolve(),
        }
        if source_digest != formal_digest:
            raise ProbeFailure("read-only generated source changed during candidate editing")
        report["candidate_loop"] = "passed"
        report["status"] = "passed"
        return report
    finally:
        if active_session:
            try:
                client.command("session", "close", "--session", active_session, "--destroy", timeout=30)
            except Exception:
                pass
        if formal is not None and formal_modes:
            for relative, mode in formal_modes.items():
                path = formal if relative == "." else formal / relative
                try:
                    path.chmod(mode)
                except OSError:
                    pass
        if server_started:
            try:
                client.command("stop", timeout=30)
            except Exception:
                pass


def main() -> int:
    try:
        report = _run()
    except Exception as exc:
        reason = _safe(str(exc))
        report = {
            "schema_version": "j00-cli-probe-v1",
            "status": "failed",
            "candidate_loop": "failed",
            "reason": reason,
            "error_class": "activation_required" if reason.startswith("CLIENT_ACTIVATION_REQUIRED:") else "probe_failure",
        }
        _write_report(report)
        print(f"J00 official CLI probe failed: {_safe(str(exc))}", file=sys.stderr)
        return 1
    _write_report(report)
    print(
        f"J00 official CLI probe {report['status']}; "
        f"candidate loop: {report.get('candidate_loop')}"
    )
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())