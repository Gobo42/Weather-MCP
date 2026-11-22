import importlib.util
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import uuid


def main():
    project = Path(__file__).resolve().parents[1]
    assert os.environ.get("WEATHER_TEST_DATABASE_CONFIG"), "explicit private fixture config required"
    socket_path = Path(os.environ["WEATHER_TEST_SOCKET"]).resolve()
    assert socket_path.is_relative_to("/build/weather-agent"), "private fixture socket required"
    fixture_file=Path(os.environ["WEATHER_TEST_DATABASE_CONFIG"]).resolve()
    assert fixture_file.is_relative_to("/build/weather-agent"), "private fixture configuration required"
    fixture_config=json.loads(fixture_file.read_text())
    connection=Path(fixture_config["mariadb_file"])
    if not connection.is_absolute(): connection=fixture_file.parent/connection
    assert Path(json.loads(connection.read_text())["socket"]).resolve()==socket_path, "fixture config/control socket mismatch"

    mcp_project = Path(os.environ.get("MCP_PROJECT", str(project.parent / "mariadb-mcp")))
    template = project / "examples/mcp/app.example.json"
    assert template.is_file(), "weather MCP profile is not implemented"
    module_spec = importlib.util.spec_from_file_location("acceptance_helpers", mcp_project / "tests/private_acceptance.py")
    helpers = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(helpers)
    binary=os.environ.get("MCP_BINARY","/build/weather-agent/mariadb-mcp-test")
    if not os.environ.get("MCP_BINARY"):
        subprocess.run(["/usr/local/go/bin/go", "build", "-o", binary, "./cmd/mariadb-mcp"], cwd=mcp_project, check=True)
    subprocess.run(["/usr/local/go/bin/go","test","-count=1","-run","^TestMCPSeedLocations$","."],cwd=project,check=True)
    with tempfile.TemporaryDirectory(dir="/build/weather-agent/test-tmp", prefix="mcp-") as temporary:
        root = Path(temporary)
        for name in ("apps.d", "connections", "credentials"):
            (root / name).mkdir(mode=0o700)
        helpers.write(root / "apps.d/weather.json", json.loads(template.read_text()))
        helpers.write(root / "connections/weather.json", {"socket": str(socket_path), "database": "weatheragent", "credentials_file": "../credentials/weather.json"})
        helpers.write(root / "credentials/weather.json", {"username": "weather_mcp", "password": "weather-fixture-mcp-password-00000000"})
        helpers.prepare_auth(binary, root / "oauth.json")
        auth = helpers.read(root / "oauth.json")
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        base = f"127.0.0.1:{port}"
        helpers.write(root / "mcp.json", {"version": 1, "listen": base, "public_base_url": "https://mcp.example.invalid/agent", "apps_directory": "apps.d", "oauth_credentials_file": "oauth.json", "state_directory": "state", "redirect_uris": ["http://127.0.0.1:9876/callback"]})
        subprocess.run([binary, "init-state", "--config", str(root / "mcp.json")], check=True, stdout=subprocess.DEVNULL)
        subprocess.run([binary, "check", "--config", str(root / "mcp.json")], check=True)
        server = helpers.start(binary, root / "mcp.json", base)
        try:
            reader = helpers.acquire(base, auth, "weather:read")["access_token"]
            writer = helpers.acquire(base, auth, "weather:write")["access_token"]
            assert len(helpers.rpc(base, reader, "tools/list")["tools"]) == 5
            assert len(helpers.rpc(base, writer, "tools/list")["tools"]) == 1
            for token, tool, required_scope in ((reader, "weather__request_backfill", "weather:write"), (writer, "weather__get_latest", "weather:read")):
                status, headers, _ = helpers.request(base, "/agent/mcp", "POST", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": tool, "arguments": {}}}), {"Authorization": "Bearer " + token, "Content-Type": "application/json", "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-11-25"})
                assert status == 403 and required_scope in headers["www-authenticate"], "independent scope consent was bypassed"
            request_id = str(uuid.uuid4())
            arguments = {"location_id":"home","request_id": request_id, "start_date": "2020-05-01", "end_date": "2020-05-01"}
            result = helpers.call(base, writer, "weather__request_backfill", arguments)
            assert result["commit"] == "committed" and result["affected_rows"] == 1
            found = helpers.call(base, reader, "weather__get_backfill", {"request_id": request_id})
            assert found["rows"][0]["state"] == "pending" and found["rows"][0]["location_id"]=="home"
            subprocess.run(["/usr/local/go/bin/go", "test", "-count=1", "-run", "^TestMCPBackfillFlow$", "."], cwd=project, env=dict(os.environ, WEATHER_TEST_REQUEST_ID=request_id), check=True)
            completed = helpers.call(base, reader, "weather__get_backfill", {"request_id": request_id})
            assert completed["rows"][0]["state"] == "completed" and completed["rows"][0]["checked_hours"] == "24"
            historical = helpers.call(base, reader, "weather__get_history", {"location_id":"home","source": "history", "start": "2020-05-01T07:00:00Z", "end": "2020-05-02T07:00:00Z"})
            assert historical["row_count"] == 24
            for tool in ("weather__get_latest", "weather__get_status"):
                helpers.call(base, reader, tool,{"location_id":"home"})
            history = helpers.call(base, reader, "weather__get_history", {"location_id":"home","source": "current", "start": "1940-01-01T00:00:00Z", "end": "1940-01-02T00:00:00Z"})
            assert history["rows"] and history["rows"][0]["observed_at"].startswith("1940-01-01T")
            for bad in ({**arguments, "end_date": "2019-01-01", "request_id": str(uuid.uuid4())}, arguments, {**arguments, "extra": "SQL"}):
                result = helpers.rpc(base, writer, "tools/call", {"name": "weather__request_backfill", "arguments": bad})
                assert result.get("isError"), "invalid or duplicate request was accepted"
            locations=helpers.call(base,reader,"weather__list_locations")
            assert {"home","east"}.issubset({row["location_id"] for row in locations["rows"]})
            east=helpers.call(base,reader,"weather__get_history",{"location_id":"east","source":"current","start":"1940-01-01T00:00:00Z","end":"1940-01-02T00:00:00Z"})
            assert east["rows"] and all(row["location_id"]=="east" and row["temperature_c"]==30 for row in east["rows"])
            for tool in ("get_latest","get_status","get_history"):
                args={"source":"current","start":"1940-01-01T00:00:00Z","end":"1940-01-02T00:00:00Z"} if tool=="get_history" else {}
                assert helpers.rpc(base,reader,"tools/call",{"name":"weather__"+tool,"arguments":args}).get("isError"), "missing location accepted"
            unknown={**arguments,"location_id":"not_registered","request_id":str(uuid.uuid4())}
            assert helpers.rpc(base,writer,"tools/call",{"name":"weather__request_backfill","arguments":unknown}).get("isError"), "foreign-key guard bypassed"
            print("PASS: unchanged MCP runtime, six tools, two-location isolation, UTC history, independent consent and guarded requests")
            assert template.read_bytes() == (mcp_project / "examples/apps.d/weather.example.json").read_bytes(), "MCP example drifted from canonical weather profile"
        finally:
            helpers.stop(server)


if __name__ == "__main__":
    main()
