# Retrieval experiments

Harness: `GRAPHANN_EVAL=1 go test -run TestEvalRetrieval -v .` — live GraphANN, throwaway
tenant/index, 24-memory corpus, 32 queries (`eval_test.go`).
Gate metric: MRR@5 (higher is better), accept a mutation only if it does not lower MRR@5
and improves a secondary metric it targets. Secondary: junk (no-answer queries returning any
hit, lower is better), distinct (share of unique documents in top 5, higher is better).

| id | mutation | MRR@5 | junk | distinct | verdict |
|----|----------|-------|------|----------|---------|
| 01 | baseline: raw dense top-k | 0.980 | 1.00 | 0.912 | base |
| 02 | oversample x4 + collapse chunks per document | 0.980 | 1.00 | 1.000 | KEEP (distinct +0.088) |
| 03 | hybrid (BM25 fusion) | 0.980 | 1.00 | 0.912 | REVERT: scores identical to dense, live server ignores the flag |
| 04 | min score 0.25 | 0.980 | 0.75 | 1.000 | superseded |
| 05 | min score 0.30 | 0.980 | 0.25 | 1.000 | superseded |
| 06 | min score 0.35 | 0.980 | 0.00 | 1.000 | KEEP (junk -1.00, MRR unchanged) |
| 07 | min score 0.40 | 0.900 | 0.00 | 0.920 | REVERT: drops weak-but-correct hits (tz, vue) |
| 08 | min score 0.45 | 0.900 | 0.00 | 0.920 | REVERT: same |

Score gap on the live index: highest no-answer top score 0.321, lowest correct top score 0.370.
The floor applies to `memory_recall` only; `memory_search` shows all results with scores.

## Dedup gate (memory_store)

Metric: exact duplicates caught out of 3 live cases (higher is better), false skips must stay 0.
Contradicting statements ("spaces over tabs") score 0.95 against each other, so the lexical
word-bigram check decides; the score only pre-filters.

| id | mutation | dups caught | false skips | verdict |
|----|----------|-------------|-------------|---------|
| 09 | baseline: score >= 0.92 (tag prefix dilutes short exact dups to 0.85-0.87) | 1/3 | 0 | base |
| 10 | score >= 0.75 + lexical check | 3/3 | 0 | KEEP |
