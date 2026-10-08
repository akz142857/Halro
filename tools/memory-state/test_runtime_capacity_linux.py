import copy
import unittest

from runtime_capacity_linux import validate_report


class RuntimeCapacityEvidenceTest(unittest.TestCase):
    def report(self):
        point = {"cgroup_max": str(2048 << 20), "rss_bytes": 32 << 20,
                 "cgroup_current_bytes": 48 << 20, "cgroup_peak_bytes": 64 << 20,
                 "cgroup_events": {"max": 0, "oom": 0, "oom_kill": 0}}
        return {"phase": "restart", "requests": 100, "events": 500,
                **{name: copy.deepcopy(point) for name in ("before", "after_gc", "after_close")}}

    def test_complete_below_ceiling_with_actual_shutdown_measurement(self):
        validate_report(self.report(), 100, "restart", 2048 << 20, 1792 << 20)

    def test_missing_shutdown_or_wrong_workload_is_not_capacity_evidence(self):
        for change in ({"events": 499}, {"phase": "seed"}, {"requests": 99}):
            with self.assertRaises(ValueError):
                validate_report({**self.report(), **change}, 100, "restart", 2048 << 20, 1792 << 20)
        report = self.report()
        del report["after_close"]
        with self.assertRaises(ValueError):
            validate_report(report, 100, "restart", 2048 << 20, 1792 << 20)

    def test_actual_limit_peak_and_pressure_counters_cannot_be_substituted(self):
        for field, value in (("cgroup_max", "max"), ("cgroup_peak_bytes", 1793 << 20),
                             ("rss_bytes", 0), ("cgroup_peak_bytes", True)):
            report = self.report()
            report["after_close"][field] = value
            with self.assertRaises(ValueError):
                validate_report(report, 100, "restart", 2048 << 20, 1792 << 20)
        for field in ("max", "oom", "oom_kill"):
            for label in ("before", "after_gc", "after_close"):
                for value in (1, -1, True):
                    report = self.report()
                    report[label]["cgroup_events"][field] = value
                    with self.assertRaises(ValueError):
                        validate_report(report, 100, "restart", 2048 << 20, 1792 << 20)


if __name__ == "__main__":
    unittest.main()
