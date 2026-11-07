Review the change for correctness: does it do what the work item asks, does its goal hold (a problem keeping it false is a finding, however old), and does it break
anything it touches? Look for wrong logic, unhandled cases, and broken callers. Report
`findings` only for defects with a concrete failure scenario; otherwise `clean`.
Documentation (the paths declared in `@docs`) isn't reviewed, even when it is in the diff.
