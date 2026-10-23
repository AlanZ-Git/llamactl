"""Thin, local real-run preflight. Observations are not permission or GPU proof.

A warning latches an initial informed-confirmation requirement. Once the operator
confirms THIS run, high/unknown metrics do not create another approval gate.
Owner pause, finite authorization and actual safety failures remain hard stops.
"""

import argparse
import datetime
import json
import pathlib
import time
import urllib.request


class Preflight:
    def __init__(self, run_id, thresholds, demand):
        required = {'cpu_percent', 'ram_percent', 'gpu_percent', 'vram_percent'}
        if not run_id or set(thresholds) != required or not demand.get('rationale'):
            raise ValueError('run identity, explicit engineering thresholds and demand rationale required')
        if any(not 0 < value <= 100 for value in thresholds.values()):
            raise ValueError('thresholds must be percentages in (0, 100]')
        self.run_id = run_id
        self.thresholds = dict(thresholds)
        self.demand = dict(demand)
        self.samples = []
        self.warning_latched = False
        self.confirmation = None

    def record(self, sample):
        sample = dict(sample)
        warnings = []
        for key, threshold in self.thresholds.items():
            value = sample.get(key)
            if value is None or sample.get('stale', True):
                warnings.append({'metric': key, 'reason': 'unknown_or_stale', 'value': value})
            elif value >= threshold:
                warnings.append({'metric': key, 'reason': 'elevated', 'value': value,
                                 'engineering_proposal_threshold': threshold})
        for metric, demand in [('ram_available_bytes', 'ram_bytes'),
                               ('vram_available_bytes', 'vram_bytes')]:
            value = sample.get(metric)
            required = self.demand.get(demand)
            if value is None or required is None:
                warnings.append({'metric': metric, 'reason': 'unknown_headroom', 'value': value})
            elif value < required:
                warnings.append({'metric': metric, 'reason': 'insufficient_headroom',
                                 'value': value, 'estimated_demand': required})
        sample['warnings'] = warnings
        self.samples.append(sample)
        if len(self.samples) > 60:
            self.samples.pop(0)
        self.warning_latched = self.warning_latched or bool(warnings)
        return self.summary()

    def confirm(self, confirmation):
        if (confirmation.get('run_id') != self.run_id
                or confirmation.get('choice') not in ('retain_coload', 'clear_resources')
                or confirmation.get('informed') is not True
                or not confirmation.get('receipt')):
            raise ValueError('explicit informed confirmation for THIS run required')
        if not self.warning_latched:
            raise ValueError('normal low-load path requires no extra approval')
        self.confirmation = dict(confirmation)

    def summary(self):
        if self.confirmation:
            state = 'confirmed_proceed'
        elif self.warning_latched:
            state = 'prepared_confirmation_required'
        else:
            state = 'normal_authorization_path'
        stamps = {s.get('sampled_at') for s in self.samples
                  if s.get('sampled_at') and not s.get('stale', True)}
        coverage = {}
        for metric in ('cpu', 'ram', 'gpu', 'vram'):
            metric_stamps = {s.get('metric_timestamps', {}).get(metric, s.get('sampled_at'))
                             for s in self.samples if not s.get('stale', True)} - {None}
            parsed = []
            for stamp in metric_stamps:
                try:
                    parsed.append(datetime.datetime.fromisoformat(stamp.replace('Z', '+00:00')).timestamp())
                except (ValueError, AttributeError):
                    pass
            parsed.sort()
            coverage[metric] = {'distinct_fresh_samples': len(metric_stamps),
                                'max_gap_ms': max(((b-a)*1000 for a, b in zip(parsed, parsed[1:])), default=None),
                                'coverage_ms': (parsed[-1]-parsed[0])*1000 if len(parsed)>1 else None}
        if state == 'normal_authorization_path' and min(c['distinct_fresh_samples'] for c in coverage.values()) < 2:
            state = 'prepared_sampling'
        return {'run_id': self.run_id, 'state': state, 'metric_coverage': coverage,
                'warning_latched': self.warning_latched,
                'confirmation': self.confirmation,
                'threshold_authority': 'engineering_proposals_not_user_limits',
                'thresholds': self.thresholds, 'demand': self.demand,
                'distinct_fresh_samples': len(stamps), 'samples': list(self.samples),
                'external_coload': 'unknown_unless_operator_records_it'}

    def allowed(self, authorized, notice_delivered, owner_paused=False, safety_failed=False):
        return bool(self.samples and authorized and notice_delivered and not owner_paused
                    and not safety_failed and (self.confirmation or
                    (not self.warning_latched and self.summary()['state'] == 'normal_authorization_path')))


def metric_value(metric):
    if (not isinstance(metric, dict) or not metric.get('available')
            or metric.get('stale') or metric.get('sampled_at') is None):
        return None
    return metric.get('value')


def flatten(snapshot):
    """Convert the existing shared sampler schema; never invoke a collector."""
    memory = snapshot.get('memory', {})
    cpu = snapshot.get('cpu', {}).get('usage', {})
    gpu = snapshot.get('gpu', {})
    devices = gpu.get('devices') or []
    total, used = metric_value(memory.get('total')), metric_value(memory.get('used'))
    device = devices[0] if len(devices) == 1 else {}
    vtotal, vused = metric_value(device.get('memory_total')), metric_value(device.get('memory_used'))
    stamps = {'cpu': cpu.get('sampled_at'),
              'ram': memory.get('used', {}).get('sampled_at'),
              'gpu': device.get('utilization', {}).get('sampled_at'),
              'vram': device.get('memory_used', {}).get('sampled_at')}
    return {'sampled_at': snapshot.get('sampled_at'), 'metric_timestamps': stamps,
            'stale': snapshot.get('stale', True),
            'cpu_percent': metric_value(cpu),
            'ram_percent': used / total * 100 if total and used is not None else None,
            'gpu_percent': metric_value(device.get('utilization')),
            'vram_percent': vused / vtotal * 100 if vtotal and vused is not None else None,
            'ram_used_bytes': used, 'ram_available_bytes': metric_value(memory.get('available')),
            'vram_used_bytes': vused, 'vram_available_bytes': metric_value(device.get('memory_free')),
            'expected_interval_ms': snapshot.get('target_interval_ms'),
            'actual_interval_ms': snapshot.get('interval_ms'),
            'reasons': {'gpu': gpu.get('reason'), 'coverage': snapshot.get('process_coverage')},
            'received_at': datetime.datetime.now(datetime.timezone.utc).isoformat()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--package', required=True, type=pathlib.Path,
                        help='private JSON: run_id, thresholds, demand, observation_url')
    parser.add_argument('--key-file', required=True, type=pathlib.Path)
    parser.add_argument('--confirmation', type=pathlib.Path)
    parser.add_argument('--samples', type=int, default=4)
    parser.add_argument('--interval', type=float, default=2.1)
    args = parser.parse_args()
    if not 2 <= args.samples <= 30 or not 2 <= args.interval <= 30:
        parser.error('bounded fresh observation window: 2..30 samples, 2..30 second interval')
    package = json.loads(args.package.read_text())
    state = Preflight(package['run_id'], package['thresholds'], package['demand'])
    # Persisted warning state is part of this private run package. Merely waiting
    # for metrics to fall cannot replace an already-triggered confirmation.
    state.warning_latched = bool(package.get('warning_latched'))
    if package.get('confirmation'):
        state.confirm(package['confirmation'])
    # Local operator-supplied receipt only; this helper never fabricates notice.
    if args.confirmation and state.warning_latched:
        state.confirm(json.loads(args.confirmation.read_text()))
    from evaluator import save_checkpoint
    key = args.key_file.read_text().strip()
    for index in range(args.samples):
        req = urllib.request.Request(package['observation_url'],
                                     headers={'Authorization': 'Bearer ' + key})
        with urllib.request.urlopen(req, timeout=10) as response:
            state.record(flatten(json.load(response)))
        package.update(warning_latched=state.warning_latched, confirmation=state.confirmation,
                       preflight=state.summary())
        save_checkpoint(args.package, package)
        if index + 1 < args.samples:
            time.sleep(args.interval)
    if args.confirmation:
        state.confirm(json.loads(args.confirmation.read_text()))
    result = state.summary()
    package.update(warning_latched=state.warning_latched, confirmation=state.confirmation,
                   preflight=result)
    save_checkpoint(args.package, package)
    result['allowed'] = state.allowed(package.get('authorized') is True,
                                      package.get('notice_delivered') is True,
                                      owner_paused=package.get('owner_paused') is True,
                                      safety_failed=package.get('safety_failed') is True)
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result['allowed'] else 2


if __name__ == '__main__':
    raise SystemExit(main())
