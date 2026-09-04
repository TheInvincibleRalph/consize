# Why the Safety Net?

For the last decade, the industry’s answer to cloud waste has been **visibility**. 

We bought tools that scan our clusters, analyze our bills, and produce massive, beautiful dashboards showing exactly how much money we are wasting. These tools will happily point out that the `checkout-api` is requesting 8 GiB of memory but only using 300 MiB, and that resizing it would save $4,000 a year.

And yet, in most organizations, that dashboard is ignored. The waste remains.

Why? **Because cost-saving tools misunderstand the engineer's dilemma.**

### The Misalignment of Incentives

Imagine you are the DevOps engineer staring at that recommendation. The tool says downsize to 1 GiB. 

If you make the change and save the company $4,000, nobody will throw you a parade. It will be a footnote in a quarterly review.

But if you make the change, and tomorrow during a traffic spike the `checkout-api` OOMKills, takes down the payment gateway, and halts revenue for an hour... **it is your fault.** 

Cost savings don’t get you promoted, but taking down production can get you fired. The risk-to-reward ratio for the engineer executing the change is completely broken. Without absolute confidence, the rational choice for any engineer is to close the dashboard, leave the resources over-provisioned, and move on.

*Over-provisioning is simply an insurance policy paid for by the CFO to protect the engineer's peace of mind.*

### Action Requires Confidence

You cannot solve cloud waste by yelling at engineers to look at a dashboard. You cannot solve it by opening Jira tickets. **You can only solve it by removing the risk of taking action.**

This is the foundational philosophy behind Consize. We realized that identifying savings is only 10% of the problem. The other 90% is giving the engineer the confidence to actually apply it. 

### Enter the Safety Net

Consize isn't a cost visibility tool; it is a **safety engine**. We built it around three uncompromisable principles:

1. **Step-wise Application:** Consize never drops a limit by 80% overnight. If a massive downsize is recommended, Consize applies it in configurable, bite-sized steps (e.g., 30% at a time).
2. **Real-time SLI Verification:** The second a change is made, Consize begins aggressively querying your Prometheus metrics. It isn't just looking at CPU; it is looking at the actual health of the application — CPU throttling, OOMKills, restart rates, and p99 latency.
3. **Instant Auto-Rollback:** If an SLI breaches its baseline during the verification window, Consize doesn't just send an alert. It immediately, automatically rolls the workload back to the exact byte-for-byte state it was in before the apply. 

### The Result: Fearless Optimization

When an engineer knows that a tool will instantly revert a bad change before a human even has to wake up for a page, the math changes. 

The question is no longer, *"Am I willing to bet my weekend on this recommendation being right?"*

The question becomes, *"Why wouldn't I click Apply and let Consize figure it out safely?"*

That is why the verifier, the SLI monitors, and the auto-rollback are not just features on our roadmap. They *are* the product. Because without the safety net, nobody jumps.

