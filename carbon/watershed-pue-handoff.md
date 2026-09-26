# Question for Watershed: was PUE removed from the Google prompt measurement?

**Status:** Possible methodology inconsistency, not a confirmed error. Prepared 2026-09-26 for the authors of *Estimating GHG Emissions from AI Use: Framework for Corporate-Level Measurement*.

## The specific question

Your Activity Tier labels its hyperscaler input and output energy intensities as **pre-PUE**, then multiplies their sum by PUE. The paper says those intensities are derived from Google's measured **0.24 Wh per median Gemini Apps text prompt**. Google's measurement explicitly **includes data-center overhead (PUE)**. We could not find a step in your paper that removes Google's PUE before deriving the token factors.

Was Google's PUE stripped from the 0.24 Wh figure before setting the 0.32 Wh/1,000 input-token and 0.96 Wh/1,000 output-token defaults? If so, could you show that intermediate calculation and the assumed input/output token split? If not, the Activity Tier may count facility overhead twice for this calibration.

## Source trail

| Claim | Where to check |
| --- | --- |
| Activity Tier: `Electricity = (I × EI_in + O × EI_out) × PUE` | [Watershed white paper, Activity Tier equation, printed p. 23](https://cdn.sanity.io/files/3ogo9b9g/production/a5c1f64ca5864e61b47e6384ee4d0ed31bc861f4.pdf#page=23) |
| Hyperscaler defaults: input **0.32 Wh/1,000 tokens**, output **0.96 Wh/1,000 tokens**, PUE **1.10**; token factors described as pre-PUE | [Watershed white paper, Table 2 and detailed example, printed pp. 24 and 29](https://cdn.sanity.io/files/3ogo9b9g/production/a5c1f64ca5864e61b47e6384ee4d0ed31bc861f4.pdf#page=24) |
| The 0.32 input value is derived from Google's **0.24 Wh** prompt, an assumed **roughly 500 tokens**, and a **3:1 decode-to-prefill** energy ratio; Google's prompt token count is undisclosed | [Watershed white paper, paragraph below Table 2 and footnote 54, printed p. 25](https://cdn.sanity.io/files/3ogo9b9g/production/a5c1f64ca5864e61b47e6384ee4d0ed31bc861f4.pdf#page=25) |
| Google's 0.24 Wh includes active accelerators **0.14 Wh**, active CPU/DRAM **0.06 Wh**, provisioned idle machines **0.02 Wh**, and data-center overhead **0.02 Wh** | [Elsworth et al., *Measuring the environmental impact of delivering AI at Google Scale*, Table 1 and §4.1](https://arxiv.org/html/2508.15734v1) |
| Google's method multiplies measured machine energy by PUE, and reports a fleet PUE of **1.09** | [Elsworth et al., §3.2 and §4.3](https://arxiv.org/html/2508.15734v1) |

## Worked check

Google's published figure is facility energy: **0.24 Wh/prompt**, including approximately **0.02 Wh/prompt** of facility overhead. If token intensities are fitted directly to that figure and subsequently multiplied by PUE **1.10**, the reconstructed prompt becomes **0.264 Wh**, around **10% above** Google's measured facility energy solely from the second PUE application.

For a concrete illustration of the additional token-mix assumption, **375 input plus 125 output tokens** (500 total) with the white paper's factors yield:

```text
375 × (0.32 Wh / 1,000) + 125 × (0.96 Wh / 1,000) = 0.24 Wh
0.24 Wh × 1.10 PUE = 0.264 Wh
```

The 375/125 split is **our illustrative inference**, not a split stated by Google or Watershed. Google does not disclose the median prompt's token counts, so the per-token factors cannot be independently recovered from the published 0.24 Wh measurement alone.

If the 0.24 Wh value is the calibration target and Google's fleet PUE of **1.09** is removed first, pre-PUE intensities under this illustrative split would be approximately **0.294 and 0.881 Wh/1,000 tokens**, respectively. This is about 9% below the published 0.32 and 0.96 values. The exact correction depends on the calibration you actually used.

## Why this matters, and what this note does not claim

- The potential issue is specific to deriving **pre-PUE** token factors from a **PUE-inclusive** prompt measurement. We have not seen the underlying worksheet, and an undocumented de-PUE adjustment could resolve it.
- The **3:1** prefill/decode ratio and **500-token** prompt length are modeling assumptions. They may be useful defaults, but Google did not measure or publish those per-token intensities for this prompt.
- This question is separate from whether training and embodied hardware emissions belong in the reporting boundary. It is also separate from the unknown energy cost of prompt-cache reads. The white paper recommends disaggregating cache-hit tokens where available but gives no numeric cache-read default.

**Requested clarification:** Please provide the derivation of the 0.32 and 0.96 Wh/1,000-token defaults, including the input/output split, any removal of Google's 1.09 PUE, and treatment of host and idle-machine energy. That would let downstream calculators use these factors without accidentally counting facility energy twice.
