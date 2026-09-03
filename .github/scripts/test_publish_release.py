"""Offline tests for publication boundaries; no credentials or network."""

import copy
import unittest
from urllib.parse import parse_qs, urlsplit

from publish_release import BRANCH, REPOSITORY, TAG, publish

SHA = "a" * 40
HEAD = "b" * 40
EVENT = {
    "repository": {"full_name": REPOSITORY},
    "workflow_run": {
        "head_repository": {"full_name": REPOSITORY},
        "event": "push", "head_branch": "main", "head_sha": SHA, "conclusion": "success",
    },
}


def check(path, sha=SHA, event="push", number=1):
    return {
        "path": f".github/workflows/{path}.yml", "head_sha": sha, "event": event,
        "run_number": number, "run_attempt": 1, "status": "completed",
        "conclusion": "success", "name": path, "html_url": f"https://example.test/{path}",
    }


class FakeAPI:
    def __init__(self):
        self.main = SHA
        self.prs = [{
            "merged_at": "2026-09-03T00:00:00Z", "merge_commit_sha": SHA,
            "head": {"sha": HEAD, "ref": BRANCH, "repo": {"full_name": REPOSITORY}},
            "base": {"ref": "main", "repo": {"full_name": REPOSITORY}},
            "html_url": "https://example.test/pr",
        }]
        self.runs = [check("verify"), check("codeql")]
        self.dependencies = [check("dependency-review", HEAD, "pull_request")]
        self.tag = None
        self.release = None
        self.writes = []
        self.main_reads = 0
        self.move_before_post = False

    def __call__(self, method, path, data=None, optional=False):
        if method == "POST":
            if path != "releases":
                raise AssertionError(f"Unexpected write: {path}")
            self.writes.append(data)
            self.tag = {"object": {"type": "commit", "sha": data["target_commitish"]}}
            self.release = {"html_url": "https://example.test/release", "draft": False, "prerelease": False}
            return self.release
        if method != "GET":
            raise AssertionError(f"Unexpected method: {method}")
        if path == "git/ref/heads/main":
            self.main_reads += 1
            if self.move_before_post and self.main_reads > 1:
                return {"object": {"sha": "c" * 40}}
            return {"object": {"sha": self.main}}
        if path.startswith("pulls?"):
            return self.prs
        if path.startswith("actions/runs?"):
            query = parse_qs(urlsplit(path).query)
            return {"workflow_runs": self.runs if query["head_sha"] == [SHA] else self.dependencies}
        if path == f"git/ref/tags/{TAG}":
            return self.tag
        if path == f"releases/tags/{TAG}":
            return self.release
        raise AssertionError(f"Unexpected API path: {path}")


class PublicationGuards(unittest.TestCase):
    def test_publish_exact_commit_with_evidence_and_versioned_links(self):
        api = FakeAPI()
        publish(EVENT, "[Upgrade](../UPGRADING.md)", api)
        self.assertEqual(len(api.writes), 1)
        payload = api.writes[0]
        self.assertEqual(payload["target_commitish"], SHA)
        self.assertEqual(payload["tag_name"], TAG)
        self.assertFalse(payload["draft"])
        self.assertIn(f"/blob/{TAG}/docs/UPGRADING.md", payload["body"])
        self.assertIn("Release commit verification", payload["body"])

    def test_untrusted_events_do_not_even_call_api(self):
        for field, value in (("event", "pull_request"), ("head_branch", "feature"), ("conclusion", "failure")):
            with self.subTest(field=field):
                event = copy.deepcopy(EVENT)
                event["workflow_run"][field] = value
                def forbidden(*args, **kwargs):
                    self.fail("Untrusted event reached API")
                publish(event, "", forbidden)
        event = copy.deepcopy(EVENT)
        event["workflow_run"]["head_repository"]["full_name"] = "someone/fork"
        publish(event, "", forbidden)

    def test_current_main_and_unique_release_merge_are_required(self):
        for field in ("main", "merge", "multiple", "fork"):
            with self.subTest(field=field):
                api = FakeAPI()
                if field == "main":
                    api.main = "c" * 40
                elif field == "merge":
                    api.prs[0]["merge_commit_sha"] = "c" * 40
                elif field == "multiple":
                    api.prs *= 2
                else:
                    api.prs[0]["head"]["repo"]["full_name"] = "someone/fork"
                publish(EVENT, "", api)
                self.assertEqual(api.writes, [])

    def test_latest_checks_must_all_succeed(self):
        for field in ("missing", "failed", "newer_pending", "dependency"):
            with self.subTest(field=field):
                api = FakeAPI()
                if field == "missing":
                    api.runs.pop()
                elif field == "failed":
                    api.runs[0]["conclusion"] = "failure"
                elif field == "newer_pending":
                    newer = check("verify", number=2)
                    newer.update(status="in_progress", conclusion=None)
                    api.runs.append(newer)
                else:
                    api.dependencies[0]["conclusion"] = "failure"
                publish(EVENT, "", api)
                self.assertEqual(api.writes, [])

    def test_existing_tag_cannot_move(self):
        api = FakeAPI()
        api.tag = {"object": {"type": "commit", "sha": "c" * 40}}
        with self.assertRaisesRegex(RuntimeError, "refusing to move"):
            publish(EVENT, "", api)
        self.assertEqual(api.writes, [])

    def test_existing_release_is_idempotent(self):
        api = FakeAPI()
        publish(EVENT, "", api)
        self.assertIn("Already published", publish(EVENT, "", api))
        self.assertEqual(len(api.writes), 1)

    def test_existing_draft_is_not_overwritten(self):
        api = FakeAPI()
        api.tag = {"object": {"type": "commit", "sha": SHA}}
        api.release = {"draft": True, "prerelease": False}
        with self.assertRaisesRegex(RuntimeError, "refusing to overwrite"):
            publish(EVENT, "", api)
        self.assertEqual(api.writes, [])

    def test_main_moving_during_checks_blocks_publication(self):
        api = FakeAPI()
        api.move_before_post = True
        with self.assertRaisesRegex(RuntimeError, "Main moved"):
            publish(EVENT, "", api)
        self.assertEqual(api.writes, [])


if __name__ == "__main__":
    unittest.main()
