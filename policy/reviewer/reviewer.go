// Copyright 2019 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reviewer

import (
	"context"
	"fmt"
	"math/rand"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/pull"
)

func findLeafChildren(result common.Result) []common.Result {
	var r []common.Result
	if len(result.Children) == 0 {
		if result.Status == common.StatusPending && result.Error == nil {
			return []common.Result{result}
		}
	} else {
		for _, c := range result.Children {
			if c == nil {
				continue
			}
			if c.Status == common.StatusPending {
				r = append(r, findLeafChildren(*c)...)
			}
		}
	}
	return r
}

// select n random values from the list of users without reuse
func selectRandomUsers(n int, users []string, r *rand.Rand) []string {
	var selections []string
	if n == 0 {
		return selections
	}
	if n >= len(users) {
		return users
	}

	selected := make(map[int]bool)
	for i := 0; i < n; i++ {
		j := 0
		for {
			// Upper bound the number of attempts to uniquely select random users to n*5
			if j > n*5 {
				// We haven't been able to select a random value, bail loudly
				panic(fmt.Sprintf("Unable to select random value for %d %d", n, len(users)))
			}
			m := r.Intn(len(users))
			if !selected[m] {
				selected[m] = true
				selections = append(selections, users[m])
				break
			}
			j++
		}
	}
	return selections
}

func selectTeamMembers(prctx pull.Context, allTeams []string, r *rand.Rand) ([]string, error) {
	randomTeam := allTeams[r.Intn(len(allTeams))]
	teamMembers, err := prctx.TeamMembers(randomTeam)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get member listing for team %s", randomTeam)
	}
	return teamMembers, nil
}

func selectOrgMembers(prctx pull.Context, allOrgs []string, r *rand.Rand) ([]string, error) {
	randomOrg := allOrgs[r.Intn(len(allOrgs))]
	orgMembers, err := prctx.OrganizationMembers(randomOrg)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get member listing for org %s", randomOrg)
	}
	return orgMembers, nil
}

func selectAdmins(ctx context.Context, prctx pull.Context, adminScope common.AdminScope, r *rand.Rand) ([]string, error) {
	logger := zerolog.Ctx(ctx)

	var adminUsers []string

	// Determine what the scope of requested admins should be
	switch adminScope {
	case common.AdminScopeUser:
		logger.Debug().Msg("Selecting admin users with direct collaboration rights")
		directCollaborators, err := prctx.DirectRepositoryCollaborators()
		if err != nil {
			return nil, errors.Wrapf(err, "Unable to get list of direct collaborators on %s", prctx.RepositoryName())
		}

		var repoAdmins []string
		for user, perm := range directCollaborators {
			if perm == common.GithubAdminPermission {
				repoAdmins = append(repoAdmins, user)
			}
		}

		adminUsers = append(adminUsers, repoAdmins...)
	case common.AdminScopeTeam:
		// Only request review for admins that are added as a team
		// Resolve all admin teams on the repo, and resolve their user membership
		logger.Debug().Msg("Selecting admin users from teams")
		teams, err := prctx.Teams()
		if err != nil {
			return nil, errors.Wrap(err, "Unable to get list of teams collaborators")
		}

		for team, perm := range teams {
			if perm == common.GithubAdminPermission {
				fullTeamName := fmt.Sprintf("%s/%s", prctx.RepositoryOwner(), team)
				admins, err := prctx.TeamMembers(fullTeamName)
				if err != nil {
					return nil, errors.Wrapf(err, "Unable to get list of members for %s", team)
				}
				adminUsers = append(adminUsers, admins...)
			}
		}
	case common.AdminScopeOrg:
		logger.Debug().Msg("Selecting admin users from the org")
		orgOwners, err := prctx.OrganizationOwners(prctx.RepositoryOwner())
		if err != nil {
			return nil, errors.Wrapf(err, "Unable to get list of org owners for %s", prctx.RepositoryOwner())
		}

		for _, o := range orgOwners {
			adminUsers = append(adminUsers, o)
		}
	default:
		// unknown option, error and don't make any assumptions
		return nil, errors.Errorf("Unknown AdminScope %s, ignoring", adminScope)
	}

	return adminUsers, nil
}

func FindRandomRequesters(ctx context.Context, prctx pull.Context, result common.Result, r *rand.Rand) ([]string, error) {
	logger := zerolog.Ctx(ctx)
	pendingLeafNodes := findLeafChildren(result)
	var requestedUsers []string

	logger.Debug().Msgf("Collecting reviewers for %d pending leaf nodes", len(pendingLeafNodes))

	for _, child := range pendingLeafNodes {
		allUsers := make(map[string]struct{})

		if len(child.ReviewRequestRule.Users) > 0 {
			for _, user := range child.ReviewRequestRule.Users {
				allUsers[user] = struct{}{}
			}
		}

		if len(child.ReviewRequestRule.Teams) > 0 {
			teamMembers, err := selectTeamMembers(prctx, child.ReviewRequestRule.Teams, r)
			if err != nil {
				logger.Warn().Err(err).Msgf("Unable to get member listing for teams, skipping team member selection")
			}
			for _, user := range teamMembers {
				allUsers[user] = struct{}{}
			}
		}

		if len(child.ReviewRequestRule.Organizations) > 0 {
			orgMembers, err := selectOrgMembers(prctx, child.ReviewRequestRule.Organizations, r)
			if err != nil {
				logger.Warn().Err(err).Msg("Unable to get member listing for org, skipping org member selection")
			}
			for _, user := range orgMembers {
				allUsers[user] = struct{}{}
			}
		}

		collaboratorsToConsider := make(map[string]string)
		allCollaborators, err := prctx.RepositoryCollaborators()
		if err != nil {
			return nil, errors.Wrap(err, "Unable to list repository collaborators")
		}

		if child.ReviewRequestRule.WriteCollaborators {
			for user, perm := range allCollaborators {
				if perm == common.GithubWritePermission {
					allUsers[user] = struct{}{}
				}
			}
		}

		// When admins are selected for review, only collect the desired set of admins instead of
		// everyone, which includes org admins
		if !child.ReviewRequestRule.Admins {
			// When not looking for admins, we want to check with all possible collaborators
			collaboratorsToConsider = allCollaborators
		} else {
			admins, err := selectAdmins(ctx, prctx, child.ReviewRequestRule.AdminScope, r)
			if err != nil {
				return nil, errors.Wrap(err, "Unable to select admins")
			}

			for _, admin := range admins {
				allUsers[admin] = struct{}{}
				collaboratorsToConsider[admin] = common.GithubAdminPermission
			}
		}

		var allUserList []string
		for u := range allUsers {
			// Remove the author and any users who aren't collaborators
			// since github will fail to assign _anyone_ if the request contains one of these
			_, ok := collaboratorsToConsider[u]
			if u != prctx.Author() && ok {
				allUserList = append(allUserList, u)
			}
		}

		logger.Debug().Msgf("Found %d total candidates for review after removing author and non-collaborators; randomly selecting %d", len(allUserList), child.ReviewRequestRule.RequiredCount)
		randomSelection := selectRandomUsers(child.ReviewRequestRule.RequiredCount, allUserList, r)
		requestedUsers = append(requestedUsers, randomSelection...)
	}

	return requestedUsers, nil
}