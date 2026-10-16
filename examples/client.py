"""Requires openai. DOUBAO_API_KEY is the key from your local CPA config."""
import os
from openai import OpenAI
client=OpenAI(base_url=os.environ.get('DOUBAO_BASE_URL','http://127.0.0.1:18322/v1'),api_key=os.environ['DOUBAO_API_KEY'],max_retries=0)
with client.responses.create(model='doubao-work-pro',input='请用两句话介绍你能做什么。',stream=True) as stream:
    for event in stream:
        if event.type=='response.output_text.delta':
            print(event.delta,end='',flush=True)
print()
