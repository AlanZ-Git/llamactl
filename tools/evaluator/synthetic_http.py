"""Runnable synthetic Client integration, invoked by TestN1PythonHTTPFixture.

Only the isolated Go fixture supplies this origin. No real model, GPU or notice.
"""
import json
import pathlib
import sys
import time
import urllib.parse

from evaluator import Client, digest, read_checkpoint


def exercise(origin, directory, key, evaluator_key):
    if urllib.parse.urlsplit(origin).hostname != '127.0.0.1':
        raise ValueError('fixture accepts loopback only')
    owner = Client(origin, 'router', 'synthetic-management-key')
    client = Client(origin, 'router', evaluator_key)
    client.call('GET', 'observation')
    directory = pathlib.Path(directory)
    scan = client.call('POST', 'inventory/scans', {'root_id': 'fixture'})
    artifacts = []
    for item in scan['candidates']:
        artifact = client.call('POST', 'inventory/verify', dict(client.envelope(),
                               scan_id=scan['scan_id'], candidate_id=item['candidate_id']))
        client.call('POST', 'inventory/register', dict(client.envelope(), artifact_id=artifact['artifact_id']))
        artifacts.append(artifact)
    assert len(artifacts) == 2 and artifacts[0]['manifest_digest'] != artifacts[1]['manifest_digest']
    artifact = artifacts[0]
    request = {'parameters': {'max_tokens': 8, 'temperature': 0,
                             'chat_template_kwargs': {'enable_thinking': False}},
               'system_prefix': 'synthetic <&> only', 'template_version': 'synthetic-v1'}
    deployment = {'context': 2048, 'gpu_layers': 0, 'flash_attention': False}
    candidate = {'candidate_id': 'one', 'artifact_id': artifact['artifact_id'],
                 'artifact_manifest_digest': artifact['manifest_digest'],
                 'deployment': deployment, 'request': request}
    candidates = [candidate,
                  dict(candidate, candidate_id='generation', request=dict(request, parameters=dict(request['parameters'], temperature=0.5))),
                  dict(candidate, candidate_id='deployment', deployment=dict(deployment, context=4096)),
                  dict(candidate, candidate_id='replacement', deployment={'context': None, 'gpu_layers': 0}),
                  dict(candidate, candidate_id='model', artifact_id=artifacts[1]['artifact_id'], artifact_manifest_digest=artifacts[1]['manifest_digest'])]
    plan = {'execution': {'candidates': candidates, 'wall_seconds': 60, 'attempt_seconds': 20,
                         'cleanup_seconds': 5, 'budget': {'requests': 5, 'tokens': 40, 'bytes': 5 << 20},
                         'notice_ref': 'synthetic-fixture-not-real-notice',
                         'not_before': int(time.time()*1000)-1000,
                         'fixture_version': 'synthetic-v1', 'purpose': 'synthetic_only',
                         'parameter_ranges': {'context': [2048, 4096], 'gpu_layers': [0, 0]}},
            'patience': 8,
            'fixtures': {c['candidate_id']: {'fixture_id': 'synthetic', 'synthetic_contains': 'synthetic',
                          'messages': [{'role': 'user', 'content': 'synthetic <&> test'}]} for c in candidates}}
    checkpoint = directory / 'evaluation.json'
    run = client.run(plan, checkpoint)
    assert run['state'] == 'succeeded' and run['cleanup_state'] == 'released', run
    # A completed run is reconciled, not dispatched again. The ACKed body survives.
    repeated = client.run(plan, checkpoint)
    assert repeated['run_id'] == run['run_id']
    assert read_checkpoint(checkpoint)['outputs']['1']['body']
    report = client.report(checkpoint)
    assert report['candidates'][0]['quality_result'] == 'synthetic_only', report
    findings = client.report(checkpoint, upload=True)
    assert client.report(checkpoint, upload=True)['digest'] == findings['digest']
    doc = {'description': 'Synthetic stdlib direct-client recipe', 'task_tags': ['synthetic'],
           'artifact_id': artifact['artifact_id'], 'artifact_manifest_digest': artifact['manifest_digest'],
           'engine_identity': 'synthetic-engine-v1', 'deployment': deployment, 'request': request,
           'conclusion_level': 'synthetic_only', 'evidence': {'run_id': run['run_id'],
           'findings_seq': findings['seq'], 'findings_digest': findings['digest'], 'candidate_id': 'one'}}
    recipes = []
    for candidate in candidates:
        document = dict(doc, artifact_id=candidate['artifact_id'],
                        artifact_manifest_digest=candidate['artifact_manifest_digest'],
                        deployment=candidate['deployment'], request=candidate['request'],
                        description='Synthetic ' + candidate['candidate_id'],
                        evidence=dict(doc['evidence'], candidate_id=candidate['candidate_id']))
        recipe = client.call('POST', 'recipes', document)
        recipe_id = recipe['recipe_id']
        client.call('POST', f'recipes/{recipe_id}/versions/1/publish',
                    {'expected_version': 1, 'conclusion_level': 'synthetic_only'})
        recipes.append(recipe)
    discovered = client.discover()
    snapshot = client.call('GET', f"recipes/{recipes[0]['recipe_id']}/versions/1/export")
    imported = client.call('POST', 'recipes/import', {'dry_run': False, 'snapshot': snapshot})
    assert imported['idempotent'] is True
    assert client.call('GET', f"recipes/{recipes[0]['recipe_id']}/versions/1")['publication_state'] == 'published'
    snapshot['recipe_id'] = 'foreign-synthetic-copy'
    preview = client.call('POST', 'recipes/import', {'dry_run': True, 'snapshot': snapshot})
    assert preview['recipe']['foreign'] is True and preview['recipe']['evidence'] is None
    foreign = client.call('POST', 'recipes/import', {'dry_run': False, 'snapshot': snapshot})
    assert foreign['recipe']['conclusion_level'] == 'unverified'
    assert client.call('POST', 'recipes/import', {'dry_run': False, 'snapshot': snapshot})['idempotent']
    try:
        client.call('POST', 'recipes/foreign-synthetic-copy/versions/1/publish',
                    {'expected_version': 1, 'conclusion_level': 'synthetic_only'})
    except urllib.error.HTTPError as error:
        assert error.code == 409
    else:
        raise AssertionError('foreign evidence inherited local trust')
    assert all(recipe['recipe_id'] in json.dumps(discovered) for recipe in recipes), discovered
    direct = Client(origin, 'router', key)  # restricted key, no evaluator daemon/state
    for method, path in [('GET', 'control'), ('POST', 'operations')]:
        try:
            direct.call(method, path, None if method == 'GET' else {})
        except urllib.error.HTTPError as error:
            assert error.code == 403, error
        else:
            raise AssertionError('direct key acquired legacy controller power')
    assert direct.discover()['recipes']
    prior = None
    for index, recipe in enumerate(recipes):
        recipe_id = recipe['recipe_id']
        checkpoint = directory / f'use-{index}.json'
        ready = direct.use(recipe_id, 1, checkpoint)
        use_id = ready['run_id']
        assert ready['model'] == candidates[index]['artifact_id']
        assert ready['request_contract'] == candidates[index]['request']
        assert ready['identity']['configured_parameters'][ready['model']] == {
            k: v for k, v in candidates[index]['deployment'].items() if v is not None}, ready['identity']
        if prior:
            assert ready['identity']['revision'] > prior['identity']['revision']
            assert ready['ready_token'] != prior['ready_token']
        assert direct.use(recipe_id, 1, checkpoint)['run_id'] == use_id
        try:
            output = direct.infer(use_id, [{'role': 'user', 'content': 'synthetic test'}])
            assert output['choices'][0]['message']['content'], output
        finally:
            released = direct.release(use_id)
        assert released['cleanup_state'] == 'released', released
        prior = ready
    owner_use = direct.use(recipes[0]['recipe_id'], 1, directory / 'owner-use.json')
    policy = owner.call('GET', '/api/v1/resource-policy', absolute=True)
    paused = owner.call('PUT', '/api/v1/resource-policy',
                         {'allow_ai': False, 'boot_id': policy['boot_id'], 'expected_revision': policy['revision']},
                         absolute=True)
    owner_result = direct.poll('service-uses/' + owner_use['run_id'],
                               lambda v: v.get('frozen_at'), time.monotonic()+45)
    assert owner_result['cleanup_state'] == 'released', owner_result
    delegation = [e for e in owner_result['events'] if e['event_type'] == 'delegated_owner']
    references = [e for e in owner_result['events'] if e['event_type'] == 'owner_release_ref']
    assert len(delegation) == len(references) == 1, owner_result['events']
    assert delegation[0]['payload']['release_operation_id'] == references[0]['payload']['release_operation_id']
    assert references[0]['payload']['owner_receipt']['ended_at'] is not None
    for field in ('boot_id', 'revision', 'process_generation', 'load_generation', 'actual_model'):
        assert delegation[0]['payload']['identity'].get(field) == owner_use['identity'].get(field), (field, delegation, owner_use['identity'])
    policy = owner.call('GET', '/api/v1/resource-policy', absolute=True)
    owner.call('PUT', '/api/v1/resource-policy',
                {'allow_ai': True, 'boot_id': policy['boot_id'], 'expected_revision': policy['revision']}, absolute=True)
    resumed = direct.use(recipes[1]['recipe_id'], 1, directory / 'after-owner.json')
    assert resumed['ready_token'] != owner_use['ready_token']
    direct.infer(resumed['run_id'], [{'role': 'user', 'content': 'synthetic after owner resume'}])
    direct.release(resumed['run_id'])
    # Real HTTP lost-dispatch/idempotency, default TTL loss, and explicit retained delivery.
    ttl_request = dict(plan['execution'], **client.envelope())
    ttl_request.update(candidates=candidates[:1], budget={'requests': 1, 'tokens': 8, 'bytes': 65536})
    ttl_run = client.call('POST', 'evaluation-runs', ttl_request)
    ttl_id = ttl_run['run_id']
    messages = plan['fixtures']['one']['messages']
    attempt = dict(client.envelope(), seq=1, candidate_id='one', fixture_id='synthetic',
                   fixture_digest=digest(messages), messages=messages)
    first_dispatch = client.call('POST', f'evaluation-runs/{ttl_id}/attempts', attempt)
    result = client.poll(f'evaluation-runs/{ttl_id}/attempts/1', lambda v: v.get('receipt'), time.monotonic()+30)
    assert result['output_state'] == 'available'
    assert client.call('POST', f'evaluation-runs/{ttl_id}/attempts', attempt)['seq'] == first_dispatch['seq']
    time.sleep(1.2)
    lost = client.call('GET', f'evaluation-runs/{ttl_id}/attempts/1')
    assert lost['output_state'] == 'output_unavailable' and 'body' not in lost, lost
    client.call('POST', f'evaluation-runs/{ttl_id}/finish', client.envelope())
    facts = client.poll(f'evaluation-runs/{ttl_id}', lambda v: v.get('frozen_at'), time.monotonic()+30)
    assert facts['budget']['reserved_requests'] == 1
    retained_plan = dict(plan, execution=dict(plan['execution'], candidates=candidates[:1],
                          retain_outputs=True, budget={'requests': 1, 'tokens': 8, 'bytes': 65536}))
    retained = client.run(retained_plan, directory / 'retained.json')
    delivery = client.call('GET', 'evaluation-runs/' + retained['run_id'] + '/attempts/1')
    assert delivery['output_state'] == 'retained' and delivery['body'], delivery
    print(json.dumps({'fixture': 'synthetic_only', 'discover': 'passed', 'run': run['state'],
                      'report': 'immutable_idempotent', 'use': 'ready', 'infer': 'identity_verified',
                      'release': released['cleanup_state'], 'checkpoint_recovery': 'no_replay_body_preserved'}))


if __name__ == '__main__':
    exercise(sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4])
