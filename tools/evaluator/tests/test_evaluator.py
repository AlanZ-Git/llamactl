import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class PreflightTests(unittest.TestCase):
    def test_warning_latches_and_confirmation_overrides_high_or_unknown(self):
        p = load('preflight')
        state = p.Preflight('run-one', {'cpu_percent': 80, 'ram_percent': 85,
                                      'gpu_percent': 80, 'vram_percent': 85},
                            {'ram_bytes': 100, 'vram_bytes': 100,
                             'rationale': 'synthetic demand; engineering proposal'})
        high = {'sampled_at': 'one', 'stale': False, 'cpu_percent': 95,
                'ram_percent': 20, 'gpu_percent': None, 'vram_percent': 90,
                'ram_available_bytes': 1000, 'vram_available_bytes': 10}
        self.assertEqual(state.record(high)['state'], 'prepared_confirmation_required')
        low = dict(high, sampled_at='two', cpu_percent=1, gpu_percent=1,
                   vram_percent=1, vram_available_bytes=1000)
        self.assertEqual(state.record(low)['state'], 'prepared_confirmation_required')
        with self.assertRaises(ValueError):
            state.confirm({'run_id': 'another', 'choice': 'retain_coload',
                           'informed': True, 'receipt': 'synthetic receipt'})
        state.confirm({'run_id': 'run-one', 'choice': 'retain_coload',
                       'informed': True, 'receipt': 'synthetic user confirmation'})
        self.assertEqual(state.record(high)['state'], 'confirmed_proceed')
        self.assertTrue(state.allowed(authorized=True, notice_delivered=True))
        self.assertFalse(state.allowed(authorized=True, notice_delivered=True, owner_paused=True))

    def test_low_load_no_universal_gate_and_duplicate_samples_not_fresh(self):
        p = load('preflight')
        state = p.Preflight('r', {'cpu_percent': 80, 'ram_percent': 85,
                                 'gpu_percent': 80, 'vram_percent': 85},
                            {'ram_bytes': 1, 'vram_bytes': 1, 'rationale': 'synthetic'})
        sample = {'sampled_at': 'one', 'stale': False, 'cpu_percent': 10,
                  'ram_percent': 10, 'gpu_percent': 10, 'vram_percent': 10,
                  'ram_available_bytes': 100, 'vram_available_bytes': 100}
        state.record(sample)
        state.record(sample)
        self.assertEqual(state.summary()['distinct_fresh_samples'], 1)
        self.assertFalse(state.allowed(authorized=True, notice_delivered=True))
        state.record(dict(sample, sampled_at='two'))
        self.assertTrue(state.allowed(authorized=True, notice_delivered=True))
        self.assertFalse(state.allowed(authorized=True, notice_delivered=False))


class EvaluatorTests(unittest.TestCase):
    def test_directional_stop_oracle_distinguishes_budget_partial(self):
        e = load('evaluator')
        self.assertEqual(e.stop_reason(3, 3, 3, [1, 2, 3], 2, 0.1), 'declared_coverage')
        self.assertEqual(e.stop_reason(1, 3, 1, [1], 2, 0.1), 'budget_exhausted')
        self.assertEqual(e.stop_reason(3, 6, 6, [1, 1.01, 1.01], 2, 0.1), 'no_practical_gain')
        self.assertIsNone(e.stop_reason(2, 6, 6, [1, 2], 2, 0.1))

    def test_use_checkpoint_reconciles_lost_acceptance_without_post(self):
        import tempfile
        e = load('evaluator')
        class Fake(e.Client):
            def call(self, method, path, body=None, **kwargs):
                if method == 'POST':
                    self.fail_post = True
                    raise AssertionError('blind switch replay')
                if path == 'control': return {'boot_id': 'boot', 'revision': 2}
                if path.startswith('service-uses?'): return {'run_id': 'original'}
                if path == 'service-uses/original': return {'run_id': 'original', 'ready_token': 'ready'}
                raise AssertionError(path)
        with tempfile.TemporaryDirectory() as directory:
            checkpoint = pathlib.Path(directory) / 'use.json'
            e.save_checkpoint(checkpoint, {'schema_version': 1, 'kind': 'switch', 'request': {
                'boot_id': 'boot', 'expected_revision': 1, 'idempotency_key': 'original-key',
                'recipe_id': 'recipe', 'version': 1}})
            result = Fake('http://127.0.0.1', 'router', 'synthetic').use('recipe', 1, checkpoint)
            self.assertEqual(result['run_id'], 'original')

    def test_output_reconciliation_preserves_body_and_checks_digest(self):
        e = load('evaluator')
        import hashlib
        body = 'synthetic <&> output'
        receipt = {'output_digest': hashlib.sha256(body.encode()).hexdigest(), 'state': 'succeeded'}
        cached = {'receipt': receipt, 'body': body, 'output_state': 'available'}
        remote = {'receipt': receipt, 'output_state': 'output_unavailable'}
        self.assertEqual(e.reconcile_output(remote, cached)['body'], body)
        with self.assertRaises(ValueError):
            e.reconcile_output(remote, dict(cached, body='changed'))
        self.assertNotIn('body', e.reconcile_output(remote, None))

    def test_report_cannot_shortlist_lost_or_unscored_body(self):
        import tempfile
        e = load('evaluator')
        candidate = {'candidate_id': 'one', 'artifact_manifest_digest': 'artifact',
                     'deployment': {}, 'request': {'parameters': {'max_tokens': 4},
                     'template_version': 'v1', 'system_prefix': ''}}
        receipt = {'candidate_id': 'one', 'attempt_id': 1, 'state': 'succeeded',
                   'generation': {'truncated': False}, 'output_bytes': 9}
        class Fake(e.Client):
            def call(self, *args, **kwargs):
                return {'run_id': 'r', 'frozen_at': 1, 'manifest_digest': 'm',
                        'attempt_receipt_set_digest': 's',
                        'events': [{'event_type': 'receipt', 'payload': receipt}]}
        with tempfile.TemporaryDirectory() as directory:
            checkpoint = pathlib.Path(directory) / 'run.json'
            e.save_checkpoint(checkpoint, {'run_id': 'r', 'request': {'candidates': [candidate]},
                                          'outputs': {}, 'stop_reason': 'declared_coverage'})
            report = Fake('http://127.0.0.1', 'router', 'synthetic').report(checkpoint)
            self.assertEqual(report['candidates'][0]['quality_result'], 'unknown')
            self.assertNotEqual(report['candidates'][0]['decision'], 'shortlisted')

    def test_checkpoint_atomic_and_private(self):
        import os
        import tempfile
        e = load('evaluator')
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'checkpoint.json'
            e.save_checkpoint(path, {'run_id': 'synthetic'})
            self.assertEqual(e.read_checkpoint(path)['run_id'], 'synthetic')
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)


if __name__ == '__main__':
    unittest.main()
