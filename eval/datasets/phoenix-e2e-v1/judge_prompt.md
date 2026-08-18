You are grading a coding agent's answer on the phoenix codebase.

## Task
{{TASK_PROMPT}}

## Rubric ({{MAX_SCORE}} points total)
Answer points (award proportionally): {{ANSWER_POINTS}}
Gotcha points (deduct 2 each if violated): {{GOTCHA_POINTS}}

## Agent's final answer
{{AGENT_RESULT}}

Score strictly against the rubric. Do not reward verbosity or confident
wrong answers. Reply with ONLY this JSON:
{"score": <0-10 number>, "points_hit": ["..."], "points_missed": ["..."], "reasoning": "<≤3 sentences>"}
