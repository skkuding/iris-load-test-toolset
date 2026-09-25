-- Read-only export of the approved benchmark problem fixtures.
--
-- Executed by scripts/aws/export-problem-fixtures.sh against the dedicated
-- codedang-iris-benchmark RDS clone through the Secrets Manager read-only
-- credential. This file contains no credentials and performs no writes.
--
-- The session is forced read-only even though the benchmark role already sets
-- default_transaction_read_only, so an accidental privilege change cannot turn
-- this export into a mutation.
--
-- Each row is emitted as one line of JSON (json_build_object escapes newlines
-- inside strings) so the caller can parse it without a fragile field delimiter.
-- The `kind` field distinguishes problem specification rows from testcase rows.
--
--   kind = "problem"   one row per approved problem
--   kind = "testcase"  one row per active (is_outdated = false) testcase
--
-- The target problems are fixed to exactly 568, 569, and 570.

SET default_transaction_read_only = on;
SET statement_timeout = '15min';

SELECT json_build_object(
  'kind', 'problem',
  'id', p.id,
  'title', p.title,
  'input_description', p.input_description,
  'output_description', p.output_description,
  'hint', p.hint,
  'eng_title', p.eng_title,
  'eng_description', p.eng_description,
  'eng_input_description', p.eng_input_description,
  'eng_output_description', p.eng_output_description,
  'eng_hint', p.eng_hint,
  'time_limit_ms', p.time_limit,
  'memory_limit_mb', p.memory_limit,
  'difficulty', p.difficulty::text,
  'source', p.source
)::text
FROM problem p
WHERE p.id IN (568, 569, 570)
ORDER BY p.id;

SELECT json_build_object(
  'kind', 'testcase',
  'problem_id', tc.problem_id,
  'testcase_id', tc.id,
  'order', tc."order",
  'score_weight', tc.score_weight,
  'is_hidden', tc.is_hidden_testcase,
  'is_outdated', tc.is_outdated,
  'input', tc.input,
  'output', tc.output
)::text
FROM problem_testcase tc
WHERE tc.problem_id IN (568, 569, 570)
  AND tc.is_outdated = false
ORDER BY tc.problem_id, tc.id;
