/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package trigger

import (
	"fmt"

	"github.com/jenkins-x/go-scm/scm"
	"github.com/sirupsen/logrus"

	"github.com/jenkins-x/lighthouse/pkg/prow/config"
	"github.com/jenkins-x/lighthouse/pkg/prow/github"
	"github.com/jenkins-x/lighthouse/pkg/prow/labels"
	"github.com/jenkins-x/lighthouse/pkg/prow/pjutil"
	"github.com/jenkins-x/lighthouse/pkg/prow/plugins"
	"k8s.io/apimachinery/pkg/util/sets"
)

func handleGenericComment(c Client, trigger *plugins.Trigger, gc github.GenericCommentEvent) error {
	org := gc.Repo.Namespace
	repo := gc.Repo.Name
	number := gc.Number
	commentAuthor := gc.Author.Login
	// Only take action when a comment is first created,
	// when it belongs to a PR,
	// and the PR is open.
	if gc.Action != scm.ActionCreate || !gc.IsPR || gc.IssueState != "open" {
		return nil
	}
	// Skip comments not germane to this plugin
	if !pjutil.RetestRe.MatchString(gc.Body) && !pjutil.OkToTestRe.MatchString(gc.Body) && !pjutil.TestAllRe.MatchString(gc.Body) {
		matched := false
		for _, presubmit := range c.Config.GetPresubmits(gc.Repo) {
			matched = matched || presubmit.TriggerMatches(gc.Body)
			if matched {
				break
			}
		}
		if !matched {
			c.Logger.Debug("Comment doesn't match any triggering regex, skipping.")
			return nil
		}
	}

	// Skip bot comments.
	botName, err := c.GitHubClient.BotName()
	if err != nil {
		return err
	}
	if commentAuthor == botName {
		c.Logger.Debug("Comment is made by the bot, skipping.")
		return nil
	}

	pr, err := c.GitHubClient.GetPullRequest(org, repo, number)
	if err != nil {
		return err
	}

	// Skip untrusted users comments.
	trusted, err := TrustedUser(c.GitHubClient, trigger, commentAuthor, org, repo)
	if err != nil {
		return fmt.Errorf("error checking trust of %s: %v", commentAuthor, err)
	}
	var l []*scm.Label
	if !trusted {
		// Skip untrusted PRs.
		l, trusted, err = TrustedPullRequest(c.GitHubClient, trigger, gc.IssueAuthor.Login, org, repo, number, nil)
		if err != nil {
			return err
		}
		if !trusted {
			resp := fmt.Sprintf("Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message.")
			c.Logger.Infof("Commenting \"%s\".", resp)
			return c.GitHubClient.CreateComment(org, repo, number, true, plugins.FormatResponseRaw(gc.Body, gc.Link, gc.Author.Login, resp))
		}
	}

	// At this point we can trust the PR, so we eventually update labels.
	// Ensure we have labels before test, because TrustedPullRequest() won't be called
	// when commentAuthor is trusted.
	if l == nil {
		l, err = c.GitHubClient.GetIssueLabels(org, repo, number)
		if err != nil {
			return err
		}
	}
	isOkToTest := HonorOkToTest(trigger) && pjutil.OkToTestRe.MatchString(gc.Body)
	if isOkToTest && !github.HasLabel(labels.OkToTest, l) {
		if err := c.GitHubClient.AddLabel(org, repo, number, labels.OkToTest); err != nil {
			return err
		}
	}
	if (isOkToTest || github.HasLabel(labels.OkToTest, l)) && github.HasLabel(labels.NeedsOkToTest, l) {
		if err := c.GitHubClient.RemoveLabel(org, repo, number, labels.NeedsOkToTest); err != nil {
			return err
		}
	}

	toTest, toSkip, err := FilterPresubmits(HonorOkToTest(trigger), c.GitHubClient, gc.Body, pr, c.Config.GetPresubmits(gc.Repo), c.Logger)
	if err != nil {
		return err
	}
	return RunAndSkipJobs(c, pr, toTest, toSkip, gc.GUID, trigger.ElideSkippedContexts)
}

func HonorOkToTest(trigger *plugins.Trigger) bool {
	return !trigger.IgnoreOkToTest
}

type GitHubClient interface {
	GetCombinedStatus(org, repo, ref string) (*scm.CombinedStatus, error)
	GetPullRequestChanges(org, repo string, number int) ([]*scm.Change, error)
}

// FilterPresubmits determines which presubmits should run. We only want to
// trigger jobs that should run, but the pool of jobs we filter to those that
// should run depends on the type of trigger we just got:
//  - if we get a /test foo, we only want to consider those jobs that match;
//    jobs will default to run unless we can determine they shouldn't
//  - if we got a /retest, we only want to consider those jobs that have
//    already run and posted failing contexts to the PR or those jobs that
//    have not yet run but would otherwise match /test all; jobs will default
//    to run unless we can determine they shouldn't
//  - if we got a /test all or an /ok-to-test, we want to consider any job
//    that doesn't explicitly require a human trigger comment; jobs will
//    default to not run unless we can determine that they should
// If a comment that we get matches more than one of the above patterns, we
// consider the set of matching presubmits the union of the results from the
// matching cases.
func FilterPresubmits(honorOkToTest bool, gitHubClient GitHubClient, body string, pr *scm.PullRequest, presubmits []config.Presubmit, logger *logrus.Entry) ([]config.Presubmit, []config.Presubmit, error) {
	org, repo, sha := pr.Base.Repo.Namespace, pr.Base.Repo.Name, pr.Base.Ref

	contextGetter := func() (sets.String, sets.String, error) {
		combinedStatus, err := gitHubClient.GetCombinedStatus(org, repo, sha)
		if err != nil {
			return nil, nil, err
		}
		failedContexts, allContexts := getContexts(combinedStatus)
		return failedContexts, allContexts, nil
	}

	filter, err := pjutil.PresubmitFilter(honorOkToTest, contextGetter, body, logger)
	if err != nil {
		return nil, nil, err
	}

	number, branch := pr.Number, pr.Base.Ref
	changes := config.NewGitHubDeferredChangedFilesProvider(gitHubClient, org, repo, number)
	return pjutil.FilterPresubmits(filter, changes, branch, presubmits, logger)
}

func getContexts(combinedStatus *scm.CombinedStatus) (sets.String, sets.String) {
	allContexts := sets.String{}
	failedContexts := sets.String{}
	if combinedStatus != nil {
		for _, status := range combinedStatus.Statuses {
			allContexts.Insert(status.Label)
			if status.State == scm.StateError || status.State == scm.StateFailure {
				failedContexts.Insert(status.Label)
			}
		}
	}
	return failedContexts, allContexts
}