"""Windows Job ownership. Only used by the native Python test supervisor."""
import ctypes
from ctypes import wintypes as w
import os
import time

from storage import StorageError


class BasicLimits(ctypes.Structure):
    _fields_ = [("process_time", ctypes.c_int64), ("job_time", ctypes.c_int64), ("flags", w.DWORD),
                ("minimum_working_set", ctypes.c_size_t), ("maximum_working_set", ctypes.c_size_t),
                ("active_process_limit", w.DWORD), ("affinity", ctypes.c_size_t),
                ("priority", w.DWORD), ("scheduling", w.DWORD)]


class IOCounters(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint64) for name in ("read_ops", "write_ops", "other_ops", "read_bytes", "write_bytes", "other_bytes")]


class ExtendedLimits(ctypes.Structure):
    _fields_ = [("basic", BasicLimits), ("io", IOCounters), ("process_memory", ctypes.c_size_t),
                ("job_memory", ctypes.c_size_t), ("peak_process_memory", ctypes.c_size_t),
                ("peak_job_memory", ctypes.c_size_t)]


class Accounting(ctypes.Structure):
    _fields_ = [("user", ctypes.c_int64), ("kernel", ctypes.c_int64),
                ("period_user", ctypes.c_int64), ("period_kernel", ctypes.c_int64),
                ("page_faults", w.DWORD), ("total_processes", w.DWORD),
                ("active_processes", w.DWORD), ("terminated_processes", w.DWORD)]


class Job:
    def _failure(self, code, operation):
        error = StorageError(code, operation)
        error.winerror = ctypes.get_last_error()
        error.operation = operation
        return error

    def __init__(self):
        if os.name != "nt":
            raise RuntimeError("Windows Job used on another platform")
        self.api = ctypes.WinDLL("kernel32", use_last_error=True)
        signatures = {
            "CreateJobObjectW": ([ctypes.c_void_p, w.LPCWSTR], w.HANDLE),
            "SetInformationJobObject": ([w.HANDLE, ctypes.c_int, ctypes.c_void_p, w.DWORD], w.BOOL),
            "QueryInformationJobObject": ([w.HANDLE, ctypes.c_int, ctypes.c_void_p, w.DWORD, ctypes.c_void_p], w.BOOL),
            "OpenProcess": ([w.DWORD, w.BOOL, w.DWORD], w.HANDLE),
            "AssignProcessToJobObject": ([w.HANDLE, w.HANDLE], w.BOOL),
            "TerminateJobObject": ([w.HANDLE, w.UINT], w.BOOL),
            "CloseHandle": ([w.HANDLE], w.BOOL),
        }
        for name, (args, result) in signatures.items():
            function = getattr(self.api, name)
            function.argtypes, function.restype = args, result
        self.handle = self.api.CreateJobObjectW(None, None)
        if not self.handle:
            raise StorageError("process_isolation_unavailable", "CreateJobObject")
        limits = ExtendedLimits()
        limits.basic.flags = 0x2000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE; no breakaway.
        if not self.api.SetInformationJobObject(self.handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            self.close()
            raise StorageError("process_isolation_unavailable", "SetInformationJobObject")

    def assign(self, pid):
        handle = self.api.OpenProcess(0x0100 | 0x0001, False, pid)
        if not handle:
            raise StorageError("process_isolation_unavailable", "OpenProcess")
        try:
            if not self.api.AssignProcessToJobObject(self.handle, handle):
                raise StorageError("process_isolation_unavailable", "AssignProcessToJobObject")
        finally:
            self.api.CloseHandle(handle)

    def active(self):
        state = Accounting()
        if not self.api.QueryInformationJobObject(self.handle, 1, ctypes.byref(state), ctypes.sizeof(state), None):
            raise self._failure("process_quiescence_unknown", "QueryInformationJobObject")
        return state.active_processes

    def terminate(self):
        if self.handle and not self.api.TerminateJobObject(self.handle, 125):
            raise self._failure("process_quiescence_unknown", "TerminateJobObject")

    def close(self):
        if self.handle:
            self.api.CloseHandle(self.handle)
            self.handle = None
