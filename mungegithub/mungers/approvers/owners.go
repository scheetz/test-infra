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

package approvers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/test-infra/mungegithub/features"
	c "k8s.io/test-infra/mungegithub/mungers/matchers/comment"
)

const (
	ownersFileName           = "OWNERS"
	ApprovalNotificationName = "ApprovalNotifier"
)

type RepoInterface interface {
	Approvers(path string) sets.String
	LeafApprovers(path string) sets.String
	FindApproverOwnersForPath(path string) string
}

type RepoAlias struct {
	repo  RepoInterface
	alias features.Aliases
}

func NewRepoAlias(repo RepoInterface, alias features.Aliases) *RepoAlias {
	return &RepoAlias{
		repo:  repo,
		alias: alias,
	}
}

func (r *RepoAlias) Approvers(path string) sets.String {
	return r.alias.Expand(r.repo.Approvers(path))
}

func (r *RepoAlias) LeafApprovers(path string) sets.String {
	return r.alias.Expand(r.repo.LeafApprovers(path))
}
func (r *RepoAlias) FindApproverOwnersForPath(path string) string {
	return r.repo.FindApproverOwnersForPath(path)
}

type Owners struct {
	filenames []string
	repo      RepoInterface
	seed      int64
}

func NewOwners(filenames []string, r RepoInterface, s int64) Owners {
	return Owners{filenames: filenames, repo: r, seed: s}
}

// GetApprovers returns a map from ownersFiles -> people that are approvers in them
func (o Owners) GetApprovers() map[string]sets.String {
	ownersToApprovers := map[string]sets.String{}

	for fn := range o.GetOwnersSet() {
		ownersToApprovers[fn] = o.repo.Approvers(fn)
	}

	return ownersToApprovers
}

// GetLeafApprovers returns a map from ownersFiles -> people that are approvers in them (only the leaf)
func (o Owners) GetLeafApprovers() map[string]sets.String {
	ownersToApprovers := map[string]sets.String{}

	for fn := range o.GetOwnersSet() {
		ownersToApprovers[fn] = o.repo.LeafApprovers(fn)
	}

	return ownersToApprovers
}

// GetAllPotentialApprovers returns the people from relevant owners files needed to get the PR approved
func (o Owners) GetAllPotentialApprovers() []string {
	approversOnly := []string{}
	for _, approverList := range o.GetLeafApprovers() {
		for approver := range approverList {
			approversOnly = append(approversOnly, approver)
		}
	}
	sort.Strings(approversOnly)
	return approversOnly
}

// GetReverseMap returns a map from people -> OWNERS files for which they are an approver
func (o Owners) GetReverseMap(approvers map[string]sets.String) map[string]sets.String {
	approverOwnersfiles := map[string]sets.String{}
	for ownersFile, approvers := range approvers {
		for approver := range approvers {
			if _, ok := approverOwnersfiles[approver]; ok {
				approverOwnersfiles[approver].Insert(ownersFile)
			} else {
				approverOwnersfiles[approver] = sets.NewString(ownersFile)
			}
		}
	}
	return approverOwnersfiles
}

func findMostCoveringApprover(allApprovers []string, reverseMap map[string]sets.String, unapproved sets.String) string {
	maxCovered := 0
	var bestPerson string
	for _, approver := range allApprovers {
		filesCanApprove := reverseMap[approver]
		if filesCanApprove.Intersection(unapproved).Len() > maxCovered {
			maxCovered = len(filesCanApprove)
			bestPerson = approver
		}
	}
	return bestPerson
}

// temporaryUnapprovedFiles returns the list of files that wouldn't be
// approved by the given set of approvers.
func (o Owners) temporaryUnapprovedFiles(approvers sets.String) sets.String {
	ap := NewApprovers(o)
	for approver := range approvers {
		ap.AddApprover(approver, "")
	}
	return ap.UnapprovedFiles()
}

// KeepCoveringApprovers finds who we should keep as suggested approvers given a pre-selection
// knownApprovers must be a subset of potentialApprovers.
func (o Owners) KeepCoveringApprovers(reverseMap map[string]sets.String, knownApprovers sets.String, potentialApprovers []string) sets.String {
	keptApprovers := sets.NewString()

	unapproved := o.temporaryUnapprovedFiles(knownApprovers)

	for _, suggestedApprover := range o.GetSuggestedApprovers(reverseMap, potentialApprovers).List() {
		if reverseMap[suggestedApprover].Intersection(unapproved).Len() != 0 {
			keptApprovers.Insert(suggestedApprover)
		}
	}

	return keptApprovers
}

// GetSuggestedApprovers solves the exact cover problem, finding an approver capable of
// approving every OWNERS file in the PR
func (o Owners) GetSuggestedApprovers(reverseMap map[string]sets.String, potentialApprovers []string) sets.String {
	ap := NewApprovers(o)
	for !ap.IsApproved() {
		newApprover := findMostCoveringApprover(potentialApprovers, reverseMap, ap.UnapprovedFiles())
		if newApprover == "" {
			glog.Errorf("Couldn't find/suggest approvers for each files. Unapproved: %s", ap.UnapprovedFiles())
			return ap.GetCurrentApproversSet()
		}
		ap.AddApprover(newApprover, "")
	}

	return ap.GetCurrentApproversSet()
}

// GetOwnersSet returns a set containing all the Owners files necessary to get the PR approved
func (o Owners) GetOwnersSet() sets.String {
	owners := sets.NewString()
	for _, fn := range o.filenames {
		owners.Insert(o.repo.FindApproverOwnersForPath(fn))
	}
	return removeSubdirs(owners.List())
}

// Shuffles the potential approvers so that we don't always suggest the same people
func (o Owners) GetShuffledApprovers() []string {
	approversList := o.GetAllPotentialApprovers()
	order := rand.New(rand.NewSource(o.seed)).Perm(len(approversList))
	people := make([]string, 0, len(approversList))
	for _, i := range order {
		people = append(people, approversList[i])
	}
	return people
}

// removeSubdirs takes a list of directories as an input and returns a set of directories with all
// subdirectories removed.  E.g. [/a,/a/b/c,/d/e,/d/e/f] -> [/a, /d/e]
func removeSubdirs(dirList []string) sets.String {
	toDel := sets.String{}
	for i := 0; i < len(dirList)-1; i++ {
		for j := i + 1; j < len(dirList); j++ {
			// ex /a/b has prefix /a so if remove /a/b since its already covered
			if strings.HasPrefix(dirList[i], dirList[j]) {
				toDel.Insert(dirList[i])
			} else if strings.HasPrefix(dirList[j], dirList[i]) {
				toDel.Insert(dirList[j])
			}
		}
	}
	finalSet := sets.NewString(dirList...)
	finalSet.Delete(toDel.List()...)
	return finalSet
}

// Approval has the information about each approval on a PR
type Approval struct {
	Login     string // Login of the approver
	How       string // How did the approver approved
	Reference string // Where did the approver approved
}

// String creates a link for the approval. Use `Login` if you just want the name.
func (a Approval) String() string {
	return fmt.Sprintf(
		`*<a href="%s" title="%s">%s</a>*`,
		a.Reference,
		a.How,
		a.Login,
	)
}

type Approvers struct {
	owners    Owners
	approvers map[string]Approval
	assignees sets.String
}

// IntersectSetsCase runs the intersection between to sets.String in a
// case-insensitive way. It returns the name with the case of "one".
func IntersectSetsCase(one, other sets.String) sets.String {
	lower := sets.NewString()
	for item := range other {
		lower.Insert(strings.ToLower(item))
	}

	intersection := sets.NewString()
	for item := range one {
		if lower.Has(strings.ToLower(item)) {
			intersection.Insert(item)
		}
	}
	return intersection
}

// NewApprovers create a new "Approvers" with no approval.
func NewApprovers(owners Owners) Approvers {
	return Approvers{
		owners:    owners,
		approvers: map[string]Approval{},
		assignees: sets.NewString(),
	}
}

// AddLGTMer adds a new LGTM Approver
func (ap *Approvers) AddLGTMer(login, reference string) {
	ap.approvers[login] = Approval{
		Login:     login,
		How:       "LGTM",
		Reference: reference,
	}
}

// AddApprover adds a new Approver
func (ap *Approvers) AddApprover(login, reference string) {
	ap.approvers[login] = Approval{
		Login:     login,
		How:       "Approved",
		Reference: reference,
	}
}

// AddSAuthorSelfApprover adds the author self approval
func (ap *Approvers) AddAuthorSelfApprover(login, reference string) {
	ap.approvers[login] = Approval{
		Login:     login,
		How:       "Author self-approved",
		Reference: reference,
	}
}

// RemoveApprover removes an approver from the list.
func (ap *Approvers) RemoveApprover(login string) {
	delete(ap.approvers, login)
}

// AddAssignees adds assignees to the list
func (ap *Approvers) AddAssignees(logins ...string) {
	ap.assignees.Insert(logins...)
}

// GetCurrentApproversSet returns the set of approvers (login only)
func (ap Approvers) GetCurrentApproversSet() sets.String {
	currentApprovers := sets.NewString()

	for approver := range ap.approvers {
		currentApprovers.Insert(approver)
	}

	return currentApprovers
}

// GetFilesApprovers returns a map from files -> list of current approvers.
func (ap Approvers) GetFilesApprovers() map[string]sets.String {
	filesApprovers := map[string]sets.String{}
	currentApprovers := ap.GetCurrentApproversSet()

	for fn, potentialApprovers := range ap.owners.GetApprovers() {
		// The order of parameter matters here:
		// - currentApprovers is the list of github handle that have approved
		// - potentialApprovers is the list of handle in OWNERSa
		// files that can approve each file.
		//
		// We want to keep the syntax of the github handle
		// rather than the potential mis-cased username found in
		// the OWNERS file, that's why it's the first parameter.
		filesApprovers[fn] = IntersectSetsCase(currentApprovers, potentialApprovers)
	}

	return filesApprovers
}

// UnapprovedFiles returns owners files that still need approval
func (ap Approvers) UnapprovedFiles() sets.String {
	unapproved := sets.NewString()
	for fn, approvers := range ap.GetFilesApprovers() {
		if len(approvers) == 0 {
			unapproved.Insert(fn)
		}
	}
	return unapproved
}

// UnapprovedFiles returns owners files that still need approval
func (ap Approvers) GetFiles(org, project string) []File {
	allOwnersFiles := []File{}
	filesApprovers := ap.GetFilesApprovers()
	for _, fn := range ap.owners.GetOwnersSet().List() {
		if len(filesApprovers[fn]) == 0 {
			allOwnersFiles = append(allOwnersFiles, UnapprovedFile{fn, org, project})
		} else {
			allOwnersFiles = append(allOwnersFiles, ApprovedFile{fn, filesApprovers[fn], org, project})
		}
	}

	return allOwnersFiles
}

// GetCCs gets the list of suggested approvers for a pull-request.  It
// now considers current assignees as potential approvers. Here is how
// it works:
// - We find suggested approvers from all potential approvers, but
// remove those that are not useful considering current approvers and
// assignees. This only uses leave approvers to find approvers the
// closest to the changes.
// - We find a subset of suggested approvers from from current
// approvers, suggested approvers and assignees, but we remove thoses
// that are not useful considering suggestd approvers and current
// approvers. This uses the full approvers list, and will result in root
// approvers to be suggested when they are assigned.
// We return the union of the two sets: suggested and suggested
// assignees.
// The goal of this second step is to only keep the assignees that are
// the most useful.
func (ap Approvers) GetCCs() []string {
	randomizedApprovers := ap.owners.GetShuffledApprovers()

	currentApprovers := ap.GetCurrentApproversSet()
	approversAndAssignees := currentApprovers.Union(ap.assignees)
	leafReverseMap := ap.owners.GetReverseMap(ap.owners.GetLeafApprovers())
	suggested := ap.owners.KeepCoveringApprovers(leafReverseMap, approversAndAssignees, randomizedApprovers)
	approversAndSuggested := currentApprovers.Union(suggested)
	everyone := approversAndSuggested.Union(ap.assignees)
	fullReverseMap := ap.owners.GetReverseMap(ap.owners.GetApprovers())
	keepAssignees := ap.owners.KeepCoveringApprovers(fullReverseMap, approversAndSuggested, everyone.List())

	return suggested.Union(keepAssignees).List()
}

// IsApproved returns a bool indicating whether or not the PR is approved
func (ap Approvers) IsApproved() bool {
	return ap.UnapprovedFiles().Len() == 0
}

// ListApprovals returns the list of approvals
func (ap Approvers) ListApprovals() []Approval {
	approvals := []Approval{}

	for _, approver := range ap.GetCurrentApproversSet().List() {
		approvals = append(approvals, ap.approvers[approver])
	}

	return approvals
}

type File interface {
	String() string
}

type ApprovedFile struct {
	filepath  string
	approvers sets.String
	org       string
	project   string
}

type UnapprovedFile struct {
	filepath string
	org      string
	project  string
}

func (a ApprovedFile) String() string {
	fullOwnersPath := filepath.Join(a.filepath, ownersFileName)
	link := fmt.Sprintf("https://github.com/%s/%s/blob/master/%v", a.org, a.project, fullOwnersPath)
	return fmt.Sprintf("- ~~[%s](%s)~~ [%v]\n", fullOwnersPath, link, strings.Join(a.approvers.List(), ","))
}

func (ua UnapprovedFile) String() string {
	fullOwnersPath := filepath.Join(ua.filepath, ownersFileName)
	link := fmt.Sprintf("https://github.com/%s/%s/blob/master/%v", ua.org, ua.project, fullOwnersPath)
	return fmt.Sprintf("- **[%s](%s)**\n", fullOwnersPath, link)
}

// GenerateTemplateOrFail takes a template, name and data, and generates
// the corresping string. nil is returned if it fails. An error is
// logged.
func GenerateTemplateOrFail(templ, name string, data interface{}) *string {
	buf := bytes.NewBufferString("")
	if messageTempl, err := template.New(name).Parse(templ); err != nil {
		glog.Errorf("Failed to generate template for %s: %s", name, err)
		return nil
	} else if err := messageTempl.Execute(buf, data); err != nil {
		glog.Errorf("Failed to execute template for %s: %s", name, err)
		return nil
	}
	message := buf.String()
	return &message
}

// getMessage returns the comment body that we want the approval-handler to display on PRs
// The comment shows:
// 	- a list of approvers files (and links) needed to get the PR approved
// 	- a list of approvers files with strikethroughs that already have an approver's approval
// 	- a suggested list of people from each OWNERS files that can fully approve the PR
// 	- how an approver can indicate their approval
// 	- how an approver can cancel their approval
func GetMessage(ap Approvers, org, project string) *string {
	message := GenerateTemplateOrFail(`This pull-request has been approved by: {{range $index, $approval := .ap.ListApprovals}}{{if $index}}, {{end}}{{$approval}}{{end}}
{{- if not .ap.IsApproved}}
We suggest the following additional approver{{if ne 1 (len .ap.GetCCs)}}s{{end}}: {{range $index, $cc := .ap.GetCCs}}{{if $index}}, {{end}}**{{$cc}}**{{end}}

Assign the PR to them by writing `+"`/assign {{range $index, $cc := .ap.GetCCs}}{{if $index}} {{end}}@{{$cc}}{{end}}`"+` in a comment when ready.
{{- end}}

<details {{if not .ap.IsApproved}}open{{end}}>
Needs approval from an approver in each of these OWNERS Files:

{{range .ap.GetFiles .org .project}}{{.}}{{end}}
You can indicate your approval by writing `+"`/approve`"+` in a comment
You can cancel your approval by writing `+"`/approve cancel`"+` in a comment
</details>`, "message", map[string]interface{}{"ap": ap, "org": org, "project": project})

	*message += getGubernatorMetadata(ap.GetCCs())

	title := GenerateTemplateOrFail("This PR is **{{if not .IsApproved}}NOT {{end}}APPROVED**", "title", ap)

	if title == nil || message == nil {
		return nil
	}

	notif := (&c.Notification{ApprovalNotificationName, *title, *message}).String()
	return &notif
}

// getGubernatorMetadata returns a JSON string with machine-readable information about approvers.
// This MUST be kept in sync with gubernator/github/classifier.py, particularly get_approvers.
func getGubernatorMetadata(toBeAssigned []string) string {
	bytes, err := json.Marshal(map[string][]string{"approvers": toBeAssigned})
	if err == nil {
		return fmt.Sprintf("\n<!-- META=%s -->", bytes)
	}
	return ""
}
