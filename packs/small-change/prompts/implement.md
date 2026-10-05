# Small change

Work on the Task in the routing block. The stage has two parts separated by a human approval.

When the routing block has an **Implementation plan** path and no **Approved implementation plan**, inspect the relevant source and instructions, then write a short, concrete plan to that path. Include the files to change, checks to run, and material risks. Do not edit source or run shell commands. Finish with a complete result.json. The orchestrator stops for approval.

When the routing block has an **Approved implementation plan**, read that exact revision and implement it in this worktree. Read the Task and project instructions again. Run the resolved project checks, report what happened, and write result.json. The plan approval permits source edits in this same stage; it does not permit changing scope or bypassing review.
