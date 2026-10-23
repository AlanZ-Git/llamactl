"""N1 stdlib client: bounded serial evaluation and direct recipe service use.

Checkpoints are private client caches. Only server run/events establish facts.
No SSH, daemon, model downloads, shell execution or automatic retry of inference.
"""

import argparse
import hashlib
import json
import os
import pathlib
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


def canonical(value):
    # Match the server's encoding/json canonical objects, including HTML escapes.
    text = json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False,
                      allow_nan=False)
    for char in ('&', '<', '>', chr(0x2028), chr(0x2029)):
        text = text.replace(char, chr(92) + 'u%04x' % ord(char))
    return text.encode()


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def save_checkpoint(path, value):
    path = pathlib.Path(path)
    fd, name = tempfile.mkstemp(prefix='.n1-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as output:
            json.dump(value, output, ensure_ascii=False, indent=2)
            output.flush()
            os.fsync(output.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def read_checkpoint(path):
    return json.loads(pathlib.Path(path).read_text(encoding='utf-8'))


def stop_reason(completed, declared, request_budget, scores, patience=2, epsilon=0.01):
    if completed >= declared:
        return 'declared_coverage'
    if completed >= request_budget:
        return 'budget_exhausted'
    if len(scores) > patience:
        earlier = max(scores[:-patience])
        if max(scores[-patience:]) <= earlier + epsilon:
            return 'no_practical_gain'
    return None


def reconcile_output(remote, cached):
    """Server receipt is authority; an ACK must not erase the private saved body."""
    result = dict(remote)
    receipt = result.get('receipt')
    if not receipt:
        return result
    source = result if 'body' in result else cached
    if source and 'body' in source:
        body = source['body']
        if (not isinstance(body, str) or
                hashlib.sha256(body.encode()).hexdigest() != receipt['output_digest']):
            raise ValueError('output digest mismatch; never reconstruct or replay')
        result['body'] = body
        if 'body' not in remote:
            result['output_state'] = 'cached_verified'
    return result


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise urllib.error.HTTPError(req.full_url, code, 'redirect refused', headers, fp)


class Client:
    def __init__(self, base_url, instance, key, timeout=15):
        parsed = urllib.parse.urlsplit(base_url)
        if parsed.scheme not in ('http', 'https') or not parsed.netloc or parsed.query or parsed.fragment:
            raise ValueError('explicit HTTP(S) manager origin required')
        if '/' in instance or not instance:
            raise ValueError('instance name required')
        self.base = base_url.rstrip('/')
        self.prefix = '/api/v1/instances/' + urllib.parse.quote(instance, safe='') + '/'
        self.instance = instance
        self.key = key
        self.timeout = min(timeout, 600)
        self.transport = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        self.response_headers = {}

    def call(self, method, path, body=None, headers=None, absolute=False):
        endpoint = self.base + (path if absolute else self.prefix + path)
        request_headers = {'Authorization': 'Bearer ' + self.key,
                           'Content-Type': 'application/json'}
        request_headers.update(headers or {})
        req = urllib.request.Request(endpoint, data=None if body is None else canonical(body),
                                     headers=request_headers, method=method)
        # No retry wrapper: a transport exception is an ambiguous execution result.
        with self.transport.open(req, timeout=self.timeout) as response:
            raw = response.read((2 << 20) + 1)
            if len(raw) > 2 << 20:
                raise ValueError('response exceeds client budget; reconcile, do not replay')
            self.response_headers = {key.lower(): value for key, value in response.headers.items()}
            return json.loads(raw)

    def envelope(self, key=None):
        status = self.call('GET', 'inventory')
        return {'boot_id': status['boot_id'], 'expected_revision': status['revision'],
                'idempotency_key': key or uuid.uuid4().hex}

    def poll(self, path, predicate, deadline, heartbeat_run=None):
        last_heartbeat = 0
        while time.monotonic() < deadline:
            result = self.call('GET', path)
            if predicate(result):
                return result
            if heartbeat_run and time.monotonic() - last_heartbeat >= 20:
                self.call('POST', 'evaluation-runs/' + heartbeat_run + '/heartbeat', self.envelope())
                last_heartbeat = time.monotonic()
            time.sleep(0.2)
        raise TimeoutError('client polling deadline; reconcile server state, do not replay inference')

    def discover(self):
        return self.call('GET', 'recipes?published=true')

    def use(self, recipe_id, version, checkpoint):
        if pathlib.Path(checkpoint).exists():
            cache = read_checkpoint(checkpoint)
            request = cache['request']
            if (cache.get('kind') != 'switch' or request['recipe_id'] != recipe_id
                    or request['version'] != version):
                raise ValueError('checkpoint belongs to another explicit recipe/version')
        else:
            request = dict(self.envelope(), recipe_id=recipe_id, version=version)
            cache = {'schema_version': 1, 'kind': 'switch', 'request': request,
                     'candidate_order': [], 'strategy_version': 'direct-v1'}
            save_checkpoint(checkpoint, cache)
        if not cache.get('run_id'):
            query = urllib.parse.urlencode({'key': request['idempotency_key'],
                                            'accepted_boot': request['boot_id']})
            try:
                accepted = self.call('GET', 'service-uses?' + query)
            except urllib.error.HTTPError as error:
                if error.code != 404:
                    raise
                if self.envelope()['boot_id'] != request['boot_id']:
                    raise ValueError('epoch changed; do not replay switch') from error
                accepted = self.call('POST', 'service-uses', request)
            cache['run_id'] = accepted['run_id']
            save_checkpoint(checkpoint, cache)
        return self.poll('service-uses/' + cache['run_id'],
                         lambda value: value.get('ready_token') or value.get('frozen_at'),
                         time.monotonic() + 600)

    def infer(self, use_id, messages):
        use = self.call('GET', 'service-uses/' + use_id)
        if not use.get('ready_token'):
            raise ValueError('service use is not ready; never re-enable an old use')
        contract = use['request_contract']
        prompt_digest = digest({'system_prefix': contract['system_prefix'],
                                'template_version': contract['template_version']})
        if prompt_digest != use['prompt_digest']:
            raise ValueError('prompt identity mismatch')
        result = self.call('POST', '/v1/chat/completions',
                           {'model': self.instance + '/' + use['model'], 'messages': messages},
                           {'X-Service-Use': use_id, 'X-Ready-Token': use['ready_token'],
                            'X-Prompt-Template-Version': contract['template_version'],
                            'X-Prompt-Digest': prompt_digest,
                            'X-Request-Timeout-Seconds': str(min(self.timeout, 600))}, absolute=True)
        identity = use['identity']
        expected = {'x-boot-id': identity['boot_id'], 'x-config-revision': str(identity['revision']),
                    'x-recipe-id': use['recipe_id'], 'x-recipe-version': str(use['recipe_version']),
                    'x-process-generation': str(identity['process_generation']),
                    'x-load-generation': str(identity['load_generation']),
                    'x-prompt-template-version': contract['template_version'], 'x-prompt-digest': prompt_digest}
        if any(self.response_headers.get(k) != v for k, v in expected.items()):
            raise ValueError('inference response identity mismatch; do not replay')
        return result

    def release(self, use_id):
        self.call('POST', 'service-uses/' + use_id + '/release', self.envelope())
        return self.poll('service-uses/' + use_id, lambda value: value.get('frozen_at'),
                         time.monotonic() + 40)

    def run(self, plan, checkpoint):
        if pathlib.Path(checkpoint).exists():
            cache = read_checkpoint(checkpoint)
            if cache.get('plan_digest') != digest(plan):
                raise ValueError('checkpoint belongs to a different immutable plan')
        else:
            request = dict(plan['execution'], **self.envelope())
            cache = {'schema_version': 1, 'kind': 'evaluation', 'request': request,
                     'plan_digest': digest(plan), 'candidate_order':
                     [candidate['candidate_id'] for candidate in request['candidates']],
                     'strategy_version': 'serial-directional-v1', 'attempts': {}, 'outputs': {}}
            save_checkpoint(checkpoint, cache)
        if not cache.get('run_id'):
            query = urllib.parse.urlencode({'key': cache['request']['idempotency_key'],
                                            'accepted_boot': cache['request']['boot_id']})
            try:
                accepted = self.call('GET', 'evaluation-runs?' + query)
            except urllib.error.HTTPError as error:
                if error.code != 404:
                    raise
                accepted = self.call('POST', 'evaluation-runs', cache['request'])
            cache['run_id'] = accepted['run_id']
            save_checkpoint(checkpoint, cache)
        run_id = cache['run_id']
        run = self.call('GET', 'evaluation-runs/' + run_id)
        if run.get('frozen_at'):
            for event in run.get('events', []):
                if event['event_type'] != 'receipt':
                    continue
                receipt = event['payload']
                key = str(receipt['attempt_id'])
                remote = self.call('GET', f'evaluation-runs/{run_id}/attempts/{key}')
                self.cache_output(cache, key, remote, plan['fixtures'][receipt['candidate_id']])
            save_checkpoint(checkpoint, cache)
            return run
        if self.envelope()['boot_id'] != cache['request']['boot_id']:
            raise ValueError('epoch changed: query history/recovery; do not replay old work')
        scores = []
        reason = None
        for index, candidate_id in enumerate(cache['candidate_order'], 1):
            if index > cache['request']['budget']['requests']:
                reason = 'budget_exhausted'
                break
            key = str(index)
            if key not in cache['attempts']:
                messages = plan['fixtures'][candidate_id]['messages']
                request = dict(self.envelope(), seq=index, candidate_id=candidate_id,
                               fixture_id=plan['fixtures'][candidate_id]['fixture_id'],
                               fixture_digest=digest(messages), messages=messages)
                cache['attempts'][key] = request
                save_checkpoint(checkpoint, cache)
            try:
                result = self.call('GET', f'evaluation-runs/{run_id}/attempts/{index}')
            except urllib.error.HTTPError as error:
                if error.code != 404:
                    raise
                self.call('POST', f'evaluation-runs/{run_id}/attempts', cache['attempts'][key])
                result = None
            if not result or not result.get('receipt'):
                result = self.poll(f'evaluation-runs/{run_id}/attempts/{index}',
                                   lambda value: value.get('receipt'),
                                   time.monotonic() + cache['request']['attempt_seconds'] + 10,
                                   heartbeat_run=run_id)
            # Cache received body BEFORE ACK. Missing body is not reconstructed.
            result = self.cache_output(cache, key, result, plan['fixtures'][candidate_id])
            save_checkpoint(checkpoint, cache)
            receipt = result['receipt']
            if result.get('output_state') in ('available', 'retained'):
                self.call('POST', f'evaluation-runs/{run_id}/attempts/{index}/ack',
                          {'digest': receipt['output_digest']})
            # Explicit synthetic oracle; never call this business/performance quality.
            score = 1 if result.get('synthetic_oracle_passed') is True else 0
            scores.append(score)
            reason = stop_reason(index, len(cache['candidate_order']),
                                 cache['request']['budget']['requests'], scores,
                                 plan.get('patience', 2), plan.get('epsilon', 0.01))
            if reason:
                break
        cache['stop_reason'] = reason or 'declared_coverage'
        save_checkpoint(checkpoint, cache)
        self.call('POST', f'evaluation-runs/{run_id}/finish', self.envelope())
        return self.poll(f'evaluation-runs/{run_id}', lambda value: value.get('frozen_at'),
                         time.monotonic() + 40)

    @staticmethod
    def cache_output(cache, key, remote, fixture):
        outputs = cache.setdefault('outputs', {})
        result = reconcile_output(remote, outputs.get(key))
        expected = fixture.get('synthetic_contains')
        receipt = result['receipt']
        result['synthetic_oracle_passed'] = bool(expected and expected in result.get('body', '')
                                                and receipt['state'] == 'succeeded'
                                                and not receipt['generation']['truncated'])
        outputs[key] = result
        return result

    def report(self, checkpoint, upload=False):
        cache = read_checkpoint(checkpoint)
        run = self.call('GET', 'evaluation-runs/' + cache['run_id'])
        if not run.get('frozen_at'):
            raise ValueError('finish execution and cleanup before analysis')
        receipts = [event['payload'] for event in run['events'] if event['event_type'] == 'receipt']
        candidates = []
        for candidate in cache['request']['candidates']:
            tested = [receipt for receipt in receipts if receipt['candidate_id'] == candidate['candidate_id']]
            ids = [receipt['attempt_id'] for receipt in tested]
            outputs = cache.get('outputs', {})
            usable = tested and all(receipt['state'] == 'succeeded' and not receipt['generation']['truncated']
                                    and receipt['output_bytes'] > 0
                                    and outputs.get(str(receipt['attempt_id']), {}).get('synthetic_oracle_passed') is True
                                    and 'body' in reconcile_output({'receipt': receipt}, outputs.get(str(receipt['attempt_id'])))
                                    for receipt in tested)
            candidates.append({'candidate_id': candidate['candidate_id'],
                               'artifact_manifest_digest': candidate['artifact_manifest_digest'],
                               'parameter_digest': digest({'deployment': candidate['deployment'],
                                                           'request': candidate['request']}),
                               'tested_attempt_ids': ids, 'tested_range': ids, 'untested_range': [],
                               'quality_result': 'synthetic_only' if usable else ('unknown' if tested else 'untested'),
                               'decision': 'shortlisted' if usable else ('eliminated' if tested else 'untested'),
                               'reason': 'External synthetic-only analysis; no business or capacity claim',
                               'capacity_interval': None, 'limitations': ['synthetic_only', 'no GPU validation']})
        report = {'schema_version': 1, 'run_id': run['run_id'], 'analysis_version': 1,
                  'idempotency_key': 'report-v1', 'manifest_digest': run['manifest_digest'],
                  'attempt_receipt_set_digest': run['attempt_receipt_set_digest'],
                  'policy_version': 'synthetic-only-v1', 'evaluator_version': 'stdlib-v1',
                  'stop_reason': cache.get('stop_reason', 'budget_exhausted'),
                  'conclusion_level': 'synthetic_only', 'candidates': candidates}
        cache['analysis_draft'] = report
        save_checkpoint(checkpoint, cache)
        if upload:
            return self.call('POST', 'evaluation-runs/' + run['run_id'] + '/analysis', report)
        return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', default='http://127.0.0.1:8080')
    parser.add_argument('--instance', default='router')
    parser.add_argument('--key-file', type=pathlib.Path)
    parser.add_argument('--timeout', type=int, default=15)
    sub = parser.add_subparsers(dest='command', required=True)
    plan = sub.add_parser('plan'); plan.add_argument('file', type=pathlib.Path)
    run = sub.add_parser('run'); run.add_argument('file', type=pathlib.Path); run.add_argument('--checkpoint', required=True)
    status = sub.add_parser('status'); status.add_argument('run_id'); status.add_argument('--kind', choices=['evaluation-runs', 'service-uses'], default='evaluation-runs')
    cancel = sub.add_parser('cancel'); cancel.add_argument('run_id')
    report = sub.add_parser('report'); report.add_argument('--checkpoint', required=True); report.add_argument('--upload', action='store_true')
    sub.add_parser('discover')
    use = sub.add_parser('use'); use.add_argument('recipe_id'); use.add_argument('version', type=int); use.add_argument('--checkpoint', required=True)
    infer = sub.add_parser('infer'); infer.add_argument('use_id'); infer.add_argument('messages', type=pathlib.Path)
    release = sub.add_parser('release'); release.add_argument('use_id')
    args = parser.parse_args()
    if args.command == 'plan':
        value = read_checkpoint(args.file)
        execution = value['execution']
        if execution.get('purpose') != 'synthetic_only' or not 1 <= len(execution['candidates']) <= 64:
            parser.error('explicit bounded synthetic candidate manifest required')
        print(json.dumps({'state': 'prepared', 'plan_digest': digest(value),
                          'candidate_order': [c['candidate_id'] for c in execution['candidates']],
                          'budget': execution['budget'], 'wall_seconds': execution['wall_seconds'],
                          'notice': 'caller must supply a real delivery receipt; this command sends none'}, indent=2))
        return
    if not args.key_file:
        parser.error('--key-file required; never place real credentials in source examples')
    client = Client(args.url, args.instance, args.key_file.read_text().strip(), args.timeout)
    if args.command == 'run': result = client.run(read_checkpoint(args.file), args.checkpoint)
    elif args.command == 'status': result = client.call('GET', args.kind + '/' + args.run_id)
    elif args.command == 'cancel': result = client.call('POST', 'evaluation-runs/' + args.run_id + '/cancel', client.envelope())
    elif args.command == 'report': result = client.report(args.checkpoint, args.upload)
    elif args.command == 'discover': result = client.discover()
    elif args.command == 'use': result = client.use(args.recipe_id, args.version, args.checkpoint)
    elif args.command == 'infer': result = client.infer(args.use_id, read_checkpoint(args.messages))
    else: result = client.release(args.use_id)
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
