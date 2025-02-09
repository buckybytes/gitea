// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issue

import (
	"context"
	"fmt"
	"time"

	issues_model "code.gitea.io/gitea/models/issues"
	org_model "code.gitea.io/gitea/models/organization"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
)

func getMergeBase(repo *git.Repository, pr *issues_model.PullRequest, baseBranch, headBranch string) (string, error) {
	// Add a temporary remote
	tmpRemote := fmt.Sprintf("mergebase-%d-%d", pr.ID, time.Now().UnixNano())
	if err := repo.AddRemote(tmpRemote, repo.Path, false); err != nil {
		return "", fmt.Errorf("AddRemote: %w", err)
	}
	defer func() {
		if err := repo.RemoveRemote(tmpRemote); err != nil {
			log.Error("getMergeBase: RemoveRemote: %v", err)
		}
	}()

	mergeBase, _, err := repo.GetMergeBase(tmpRemote, baseBranch, headBranch)
	return mergeBase, err
}

func PullRequestCodeOwnersReview(ctx context.Context, pull *issues_model.Issue, pr *issues_model.PullRequest) error {
	files := []string{"CODEOWNERS", "docs/CODEOWNERS", ".gitea/CODEOWNERS"}

	if pr.IsWorkInProgress(ctx) {
		return nil
	}

	if err := pr.LoadHeadRepo(ctx); err != nil {
		return err
	}

	if pr.HeadRepo.IsFork {
		return nil
	}

	if err := pr.LoadBaseRepo(ctx); err != nil {
		return err
	}

	repo, err := gitrepo.OpenRepository(ctx, pr.BaseRepo)
	if err != nil {
		return err
	}
	defer repo.Close()

	commit, err := repo.GetBranchCommit(pr.BaseRepo.DefaultBranch)
	if err != nil {
		return err
	}

	var data string
	for _, file := range files {
		if blob, err := commit.GetBlobByPath(file); err == nil {
			data, err = blob.GetBlobContent(setting.UI.MaxDisplayFileSize)
			if err == nil {
				break
			}
		}
	}

	rules, _ := issues_model.GetCodeOwnersFromContent(ctx, data)

	// get the mergebase
	mergeBase, err := getMergeBase(repo, pr, git.BranchPrefix+pr.BaseBranch, pr.GetGitRefName())
	if err != nil {
		return err
	}

	// https://github.com/go-gitea/gitea/issues/29763, we need to get the files changed
	// between the merge base and the head commit but not the base branch and the head commit
	changedFiles, err := repo.GetFilesChangedBetween(mergeBase, pr.HeadCommitID)
	if err != nil {
		return err
	}

	uniqUsers := make(map[int64]*user_model.User)
	uniqTeams := make(map[string]*org_model.Team)
	for _, rule := range rules {
		for _, f := range changedFiles {
			if (rule.Rule.MatchString(f) && !rule.Negative) || (!rule.Rule.MatchString(f) && rule.Negative) {
				for _, u := range rule.Users {
					uniqUsers[u.ID] = u
				}
				for _, t := range rule.Teams {
					uniqTeams[fmt.Sprintf("%d/%d", t.OrgID, t.ID)] = t
				}
			}
		}
	}

	for _, u := range uniqUsers {
		if u.ID != pull.Poster.ID {
			if _, err := issues_model.AddReviewRequest(ctx, pull, u, pull.Poster); err != nil {
				log.Warn("Failed add assignee user: %s to PR review: %s#%d, error: %s", u.Name, pr.BaseRepo.Name, pr.ID, err)
				return err
			}
		}
	}
	for _, t := range uniqTeams {
		if _, err := issues_model.AddTeamReviewRequest(ctx, pull, t, pull.Poster); err != nil {
			log.Warn("Failed add assignee team: %s to PR review: %s#%d, error: %s", t.Name, pr.BaseRepo.Name, pr.ID, err)
			return err
		}
	}

	return nil
}
