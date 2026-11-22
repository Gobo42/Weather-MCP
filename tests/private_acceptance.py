import json
import os
from pathlib import Path
import subprocess
import sys


def main():
    project = Path(__file__).resolve().parents[1]
    filename = os.environ.get("WEATHER_TEST_DATABASE_CONFIG")
    socket_name = os.environ.get("WEATHER_TEST_SOCKET")
    if not filename or not socket_name:
        raise SystemExit("Explicit WEATHER_TEST_DATABASE_CONFIG and WEATHER_TEST_SOCKET are required; no production fallback")
    socket_path = Path(socket_name).resolve()
    assert socket_path.is_relative_to("/build/weather-agent") and socket_path.is_socket(), "private /build test socket required"
    config = json.loads(Path(filename).read_text())
    fixture_file=Path(os.environ["WEATHER_TEST_DATABASE_CONFIG"]).resolve()
    assert fixture_file.is_relative_to("/build/weather-agent"), "private fixture configuration required"
    fixture_config=json.loads(fixture_file.read_text())
    connection=Path(fixture_config["mariadb_file"])
    if not connection.is_absolute(): connection=fixture_file.parent/connection
    assert Path(json.loads(connection.read_text())["socket"]).resolve()==socket_path, "fixture config/control socket mismatch"

    locations=config.get("locations",[config.get("location",{})])
    assert locations[0]["id"]=="home" and locations[0]["latitude"]==40 and locations[0]["longitude"]==-105, "synthetic fixture location required"
    environment = dict(os.environ, GOCACHE="/build/cache", GOMODCACHE="/build/mod", GOTMPDIR="/build/tmp", TMPDIR="/build/tmp")
    subprocess.run([sys.executable, "tests/test_mcp.py"], cwd=project, env=environment, check=True)
    subprocess.run(["/usr/local/go/bin/go", "test", "-race", "-count=1", "-v", "./..."], cwd=project, env=environment, check=True)
    subprocess.run(["/usr/local/go/bin/go", "vet", "./..."], cwd=project, env=environment, check=True)
    print("PASS: mandatory private MariaDB, race, vet and actual MCP acceptance; no live provider or production changes")


if __name__ == "__main__":
    main()
