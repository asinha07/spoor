# Grading rubric

Committed before any arm-B run. `grade.py` applies it mechanically to the final `ROOT CAUSE:` line.

An answer is **correct** when both hold:

1. **Service:** the line names the ground-truth service (case-insensitive). Naming a caller that is only affected
   (for example `checkout` when `payment` is failing) is wrong.
2. **Mechanism:** the line contains at least one keyword for the scenario.

| Scenario | Service | Mechanism keywords (any) |
| --- | --- | --- |
| payment-failure | payment | charge, card, processor, 503, payment fail, declin |
| catalog-one-product | product-catalog | OLJCESPC7Z, one product, single product, specific product, product id, feature flag |
| ad-high-cpu | ad | cpu |
| recommendation-cache-leak | recommendation | memory, leak, cache |
| kafka-queue-problems | accounting | lag, kafka, consumer, queue, backlog |
| control | none | the line says `none` (or "no problem" / "no root cause") |

An answer with no `ROOT CAUSE:` line is incorrect. Partial credit is reported separately (service right, mechanism
missing) but does not count as correct.
