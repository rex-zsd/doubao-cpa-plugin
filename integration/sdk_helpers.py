from concurrent.futures import ThreadPoolExecutor, as_completed
import json,secrets,threading,time
NAMES=["lookup_alpha","lookup_beta","lookup_gamma"]
DELAYS=dict(zip(NAMES,[1.5,.2,.7]))

def tools(protocol):
    result = []
    for name in NAMES:
        fn = {'name': name, 'description': 'Independent read-only lookup; return unpredictable client value.',
              'parameters': {'type': 'object', 'properties': {'challenge': {'type': 'string'}},
                             'required': ['challenge'], 'additionalProperties': False}}
        result.append({'type': 'function', **fn} if protocol == 'responses' else {'type': 'function', 'function': fn})
    return result

def collect(client, protocol, history, key=None):
    kwargs = {'model': 'doubao-work-pro', 'tools': tools(protocol), 'stream': True,
              'parallel_tool_calls': True, 'tool_choice': 'auto'}
    if key:
        kwargs['extra_headers'] = {'Idempotency-Key': key}
    if protocol == 'responses':
        final, event_count = None, 0
        with client.responses.create(input=history, **kwargs) as stream:
            for event in stream:
                event_count += 1
                if event.type in ('error', 'response.failed', 'response.incomplete'):
                    raise RuntimeError('Responses stream failed: ' + event.type)
                if event.type == 'response.completed':
                    final = event.response
        assert final is not None, 'missing response.completed'
        calls = [{'id': x.call_id, 'name': x.name, 'arguments': x.arguments}
                 for x in final.output if x.type == 'function_call']
        output = [x.model_dump(exclude_none=True) for x in final.output]
        return calls, final.output_text, output, event_count
    accumulated, text, event_count, reason = {}, '', 0, None
    with client.chat.completions.create(messages=history, **kwargs) as stream:
        for chunk in stream:
            event_count += 1
            for choice in chunk.choices:
                reason = choice.finish_reason or reason
                text += choice.delta.content or ''
                for delta in choice.delta.tool_calls or []:
                    item = accumulated.setdefault(delta.index, {'id': '', 'name': '', 'arguments': ''})
                    item['id'] += delta.id or ''
                    if delta.function:
                        item['name'] += delta.function.name or ''
                        item['arguments'] += delta.function.arguments or ''
    calls = [accumulated[i] for i in sorted(accumulated)]
    assert reason == ('tool_calls' if calls else 'stop'), reason
    output = [{'role': 'assistant', 'content': None, 'tool_calls': [
        {'id': x['id'], 'type': 'function', 'function': {'name': x['name'], 'arguments': x['arguments']}}
        for x in calls]}] if calls else [{'role': 'assistant', 'content': text}]
    return calls, text, output, event_count

def execute_parallel(calls, challenge):
    assert len(calls) == 3 and {c['name'] for c in calls} == set(NAMES), 'model did not submit all 3 calls in one batch'
    assert len({c['id'] for c in calls}) == 3, 'duplicate call IDs'
    barrier, lock = threading.Barrier(3), threading.Lock()
    timeline, values, finished = [], {}, []
    started = time.monotonic()
    def execute(call):
        assert json.loads(call['arguments']) == {'challenge': challenge}
        barrier.wait(timeout=5)
        begin = time.monotonic()
        value = call['name'].removeprefix('lookup_') + '_' + secrets.token_hex(16)
        time.sleep(DELAYS[call['name']])
        end = time.monotonic()
        with lock:
            values[call['name'].removeprefix('lookup_')] = value
            timeline.append({'name': call['name'], 'start_ms': round((begin-started)*1000),
                             'end_ms': round((end-started)*1000)})
        return call, json.dumps({'value': value})
    with ThreadPoolExecutor(max_workers=3) as pool:
        for future in as_completed([pool.submit(execute, c) for c in calls]):
            finished.append(future.result())
    latest_start = max(x['start_ms'] for x in timeline)
    earliest_end = min(x['end_ms'] for x in timeline)
    assert latest_start < earliest_end, 'client functions did not overlap'
    return finished, values, {'tool_calls_in_one_response': len(calls), 'caller_execution_overlapped': True,
        'execution_ms': round((time.monotonic()-started)*1000), 'sum_of_serial_delays_ms': 2400,
        'returned_result_order': [x[0]['name'] for x in finished], 'timeline': timeline,
        'values_generated_after_real_model_calls': True}
