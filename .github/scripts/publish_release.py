"""One-version publisher: only the verified merge of the final release branch."""

import json
import os
import re
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

REPOSITORY = "krav01/usage-billing"
TAG = "v0.3.0"
BRANCH = "release/v0.3.0-final"


def github(method, path, data=None, optional=False):
    request = Request(
        f"https://api.github.com/repos/{REPOSITORY}/{path}",
        data=json.dumps(data).encode() if data is not None else None,
        method=method,
        headers={
            "Authorization": f"Bearer {os.environ['GH_TOKEN']}",
            "Accept": "application/vnd.github+json",
            "Content-Type": "application/json",
            "X-GitHub-Api-Version": "2026-03-10",
        },
    )
    try:
        with urlopen(request, timeout=20) as response:
            return json.load(response)
    except HTTPError as error:
        if optional and error.code == 404:
            return None
        raise RuntimeError(f"GitHub API returned HTTP {error.code}") from None


def successful_run(runs, path, sha, event):
    matching = [
        run for run in runs
        if run["path"] == path and run["head_sha"] == sha and run["event"] == event
    ]
    if not matching:
        return None
    latest = max(matching, key=lambda run: (run["run_number"], run["run_attempt"]))
    if latest["status"] != "completed" or latest["conclusion"] != "success":
        return None
    return latest


def publish(event, notes, api=github):
    run = event.get("workflow_run", {})
    if (
        event.get("repository", {}).get("full_name") != REPOSITORY
        or run.get("head_repository", {}).get("full_name") != REPOSITORY
        or run.get("event") != "push"
        or run.get("head_branch") != "main"
        or run.get("conclusion") != "success"
    ):
        return "Skipped: unrelated event"
    sha = run.get("head_sha", "")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise RuntimeError("Invalid release commit")
    if api("GET", "git/ref/heads/main")["object"]["sha"] != sha:
        return "Skipped: main moved"
    query = urlencode({"state": "closed", "base": "main", "head": f"krav01:{BRANCH}", "per_page": 100})
    prs = api("GET", f"pulls?{query}")
    if len(prs) != 1:
        return "Skipped: not the unique release branch merge"
    # List responses may omit merge metadata. Fetch the authoritative PR detail.
    pr = api("GET", f"pulls/{prs[0]['number']}")
    if not pr["merged"]:
        return "Skipped: not the unique release branch merge"
    if (
        pr["head"]["ref"] != BRANCH or pr["head"]["repo"]["full_name"] != REPOSITORY
        or pr["base"]["ref"] != "main" or pr["base"]["repo"]["full_name"] != REPOSITORY
    ):
        return "Skipped: unexpected release PR source"
    # Bind the release to the actual Git merge object. PR responses can omit
    # merge_commit_sha; the second parent must still be the exact reviewed head.
    commit = api("GET", f"git/commits/{sha}")
    parents = commit["parents"]
    if commit["sha"] != sha or len(parents) != 2 or parents[1]["sha"] != pr["head"]["sha"]:
        return "Skipped: commit does not merge the reviewed release head"
    query = urlencode({"head_sha": sha, "event": "push", "per_page": 100})
    runs = api("GET", f"actions/runs?{query}")["workflow_runs"]
    verified = []
    for path in (".github/workflows/verify.yml", ".github/workflows/codeql.yml"):
        check = successful_run(runs, path, sha, "push")
        if not check:
            return "Skipped: release commit checks are not yet all successful"
        verified.append(check)
    query = urlencode({"head_sha": pr["head"]["sha"], "event": "pull_request", "per_page": 100})
    runs = api("GET", f"actions/runs?{query}")["workflow_runs"]
    dependency = successful_run(
        runs, ".github/workflows/dependency-review.yml", pr["head"]["sha"], "pull_request"
    )
    if not dependency:
        return "Skipped: dependency review is not successful"
    verified.append(dependency)
    tag = api("GET", f"git/ref/tags/{TAG}", optional=True)
    if tag and (tag["object"]["type"] != "commit" or tag["object"]["sha"] != sha):
        raise RuntimeError("Existing tag differs; refusing to move it")
    release = api("GET", f"releases/tags/{TAG}", optional=True)
    if release:
        if not tag or release["draft"] or release["prerelease"]:
            raise RuntimeError("Existing release requires manual review; refusing to overwrite it")
        return f"Already published: {release['html_url']}"
    if api("GET", "git/ref/heads/main")["object"]["sha"] != sha:
        raise RuntimeError("Main moved before publication")
    notes = notes.replace("(../", f"(https://github.com/{REPOSITORY}/blob/{TAG}/docs/")
    notes += f"\n## Release commit verification\n\nRelease target: `{sha}`.\n\n"
    notes += "\n".join(f"- [{check['name']}]({check['html_url']}): success." for check in verified)
    notes += f"\n\n[Release PR]({pr['html_url']}).\n"
    release = api("POST", "releases", {
        "tag_name": TAG,
        "target_commitish": sha,
        "name": f"Usage Billing {TAG}",
        "body": notes,
        "draft": False,
        "prerelease": False,
        "make_latest": "true",
    })
    tag = api("GET", f"git/ref/tags/{TAG}")
    if tag["object"]["type"] != "commit" or tag["object"]["sha"] != sha:
        raise RuntimeError("Published tag verification failed")
    return f"Published: {release['html_url']}"


if __name__ == "__main__":
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    notes = Path("docs/releases/v0.3.0.md").read_text()
    print(publish(event, notes))
