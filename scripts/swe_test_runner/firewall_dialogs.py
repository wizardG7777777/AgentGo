"""Read-only visible-window guard. Never clicks Windows security prompts."""
import ctypes
import threading

from storage import StorageError


def visible_windows():
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
        raise StorageError("firewall_dialog_observation_failed")
    return found


class DialogWatch:
    def __init__(self):
        self.baseline = visible_windows()
        self.error = None
        self.detected = {}
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self._watch, daemon=True)
        self.thread.start()

    def _watch(self):
        while not self.stopped.wait(0.2):
            try:
                self.inspect()
            except Exception as error:
                self.error = error if isinstance(error, StorageError) else StorageError("firewall_dialog_observation_failed")
                return

    def inspect(self):
        new = {key: value for key, value in visible_windows().items() if key not in self.baseline}
        if new:
            self.detected.update(new)
            raise StorageError("firewall_prompt_detected")

    def check(self):
        if self.error:
            raise self.error

    def close(self):
        self.stopped.set()
        self.thread.join(timeout=5)
        if self.thread.is_alive():
            raise StorageError("firewall_dialog_monitor_not_stopped")
