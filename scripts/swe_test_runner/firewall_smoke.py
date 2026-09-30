"""Small real Windows network/privilege check before starting an unattended batch."""
from pathlib import Path
from types import SimpleNamespace
import ctypes
import json
import os
import threading
import time

import lifecycle
import processes
import storage

GO_TEST = r'''package firewallfixture
import (
 "io"
 "net"
 "net/http"
 "os"
 "os/exec"
 "strings"
 "strconv"
 "testing"
 "time"
)
func TestLoopbackAndNormalPrivileges(t *testing.T) {
 cmd:=exec.Command("powershell.exe","-NoProfile","-NonInteractive","-Command",`([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)`)
 b,err:=cmd.Output();if err!=nil || strings.TrimSpace(string(b))!="False" {t.Fatalf("test process unexpectedly elevated or privilege check failed")}
 exe,err:=os.Executable();if err!=nil {t.Fatal(err)}
 rule:=exec.Command("powershell.exe","-NoProfile","-NonInteractive","-Command",`$f=New-Object -ComObject HNetCfg.FwPolicy2;$r=$f.Rules.Item($env:SWE_WINDOWS_FIREWALL_RULE_NAME);if(-not $r.Enabled -or $r.Action -ne 1 -or $r.Direction -ne 1 -or $r.Profiles -ne 7 -or $r.ApplicationName -ine $env:SWE_FW_EXPECT_PROGRAM){exit 2};if($r.RemoteAddresses -ne '127.0.0.1/255.255.255.255'){exit 3};Write-Output verified`)
 rule.Env=append(os.Environ(),"SWE_FW_EXPECT_PROGRAM="+exe)
 b,err=rule.CombinedOutput();if err!=nil || strings.TrimSpace(string(b))!="verified" {t.Fatalf("exact program rule not active before listen: %v %s",err,b)}
 t.Log("SWE_FIREWALL_PROGRAM="+exe)
 client:=http.Client{Transport:&http.Transport{},Timeout:5*time.Second}
 defer client.CloseIdleConnections()
 for _,network:=range []string{"tcp4","tcp6"} {
  address,host:="0.0.0.0:0","127.0.0.1";if network=="tcp6" {address,host="[::]:0","::1"}
  l,err:=net.Listen(network,address);if err!=nil {t.Fatal(err)}
  s:=http.Server{Handler:http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){io.WriteString(w,"ok")})}
  defer s.Close();go s.Serve(l)
  port:=l.Addr().(*net.TCPAddr).Port
  r,err:=client.Get("http://"+net.JoinHostPort(host,strconv.Itoa(port)));if err!=nil {t.Fatal(err)}
  data,_:=io.ReadAll(r.Body);r.Body.Close();if string(data)!="ok" {t.Fatal("response mismatch")}
 }
 for _,network:=range []string{"udp4","udp6"} {
  address:="0.0.0.0:0";if network=="udp6" {address="[::]:0"}
  socket,err:=net.ListenPacket(network,address);if err!=nil {t.Fatal(err)};socket.Close()
 }
}
'''


def visible_firewall_windows():
    """Read-only top-level dialog observation; does not click or dismiss UI."""
    user32 = ctypes.WinDLL("user32", use_last_error=True)
    callback_type = ctypes.WINFUNCTYPE(ctypes.c_int, ctypes.c_void_p, ctypes.c_void_p)
    user32.EnumWindows.argtypes = [callback_type, ctypes.c_void_p]
    user32.EnumWindows.restype = ctypes.c_int
    user32.IsWindowVisible.argtypes = [ctypes.c_void_p]
    user32.GetWindowTextLengthW.argtypes = [ctypes.c_void_p]
    user32.GetWindowTextW.argtypes = [ctypes.c_void_p, ctypes.c_wchar_p, ctypes.c_int]
    found = {}
    def visit(window, unused):
        if user32.IsWindowVisible(window):
            length = min(user32.GetWindowTextLengthW(window), 2048)
            text = ctypes.create_unicode_buffer(length + 1)
            user32.GetWindowTextW(window, text, length + 1)
            if any(word in text.value.lower() for word in ("firewall", "防火墙", "windows security", "windows 安全")):
                found[int(window)] = text.value
        return 1
    callback = callback_type(visit)
    if not user32.EnumWindows(callback, None):
        raise storage.StorageError("firewall_dialog_observation_failed")
    return found


def run(root, firewall):
    if os.name != "nt":
        raise storage.StorageError("windows_firewall_wrong_platform")
    session = lifecycle.Session(root, storage.StoragePolicy(), {"mode": "firewall-validation", "provider_calls": 0})
    session.storage.firewall = firewall
    config = SimpleNamespace(suite=SimpleNamespace(suite_id="firewall-validation"))
    baseline = firewall.request("inspect_global")
    programs = []
    evidence = []
    def check(attempt):
        (attempt.worktree / "go.mod").write_text("module firewallfixture/gin\n\ngo 1.25.0\n", encoding="utf-8")
        (attempt.worktree / "network_test.go").write_text(GO_TEST, encoding="utf-8")
        before = visible_firewall_windows()
        stopped = threading.Event()
        dialogs = []
        def watch():
            while not stopped.wait(0.2):
                try:
                    fresh = visible_firewall_windows()
                    new = {key: value for key, value in fresh.items() if key not in before}
                    if new:
                        dialogs.extend(new.values())
                        raise storage.StorageError("firewall_prompt_detected")
                except Exception as error:
                    with attempt._mutex:
                        attempt._violation = error if isinstance(error, storage.StorageError) else storage.StorageError("firewall_dialog_observation_failed")
                    return
        watcher = threading.Thread(target=watch, daemon=True)
        watcher.start()
        try:
            result = processes.run(["go", "test", "-count=1", "-v", "."], cwd=attempt.worktree,
                                   env=dict(os.environ, GOTOOLCHAIN="local"), timeout=120)
        finally:
            stopped.set()
            watcher.join(timeout=5)
            storage.atomic_json(attempt.reports / "visible-dialogs.json", {"new_firewall_windows": dialogs})
        text = processes.output_summary(result).decode("utf-8", errors="replace")
        if "PASS" not in text or dialogs:
            raise RuntimeError("firewall smoke did not pass")
        values = [line.split("SWE_FIREWALL_PROGRAM=", 1)[1].strip() for line in text.splitlines() if "SWE_FIREWALL_PROGRAM=" in line]
        if len(values) != 1 or Path(values[0]).name != "gin.test.exe":
            raise RuntimeError("temporary Gin-named test executable evidence missing")
        programs.extend(values)
        return {"passed": True}
    for index in range(3):
        session.execute(config, "go-network-" + str(index), "firewall-validation", check)
        state = storage.read_json(Path(session.rows[-1]["attempt"]) / "attempt.json")
        if (not state["firewall"]["cleanup_complete"] or state["firewall"].get("leases", {}).get("acquired", 0) < 1
                or firewall.request("ping").get("active_rules") != 0):
            raise RuntimeError("firewall lease or rule cleanup evidence missing")
        evidence.append({"attempt": str(session.rows[-1]["attempt"]), "firewall": state["firewall"]})
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline:
        firewall.check_dialogs()
        time.sleep(0.1)
    after = firewall.request("inspect_global")
    if len(set(programs)) != 3 or after["profiles"] != baseline["profiles"]:
        raise RuntimeError("distinct executable paths or unchanged global firewall policy not verified")
    result = {"passed": True, "batch_id": session.batch_id, "provider_calls": 0, "programs": programs,
              "global_policy_unchanged": True, "current_profiles_before": baseline["current_profiles"],
              "current_profiles_after": after["current_profiles"], "visible_dialog_monitor": "win32-top-level", "attempts": evidence}
    storage.atomic_json(session.batch_dir / "firewall-validation.json", result)
    return result
