package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-billy/v5"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/sirupsen/logrus"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const (
	sourceRemote = "upstream"
	targetRemote = "origin"
)

var (
	workdir    = flag.String("workdir", ".", "Workdir to use")
	sourceRepo = flag.String("source-repo", "https://github.com/kubernetes/kubernetes.git", "Source repo")
	targetRepo = flag.String("target-repo", "", "Target repo")
)

func remoteTags(r *gogit.Repository, remote string) (map[string]plumbing.Hash, error) {
	refs, err := r.Storer.IterReferences()
	if err != nil {
		return nil, err
	}
	defer refs.Close()
	tagCommits := map[string]plumbing.Hash{}
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.SymbolicReference && ref.Name().IsTag() {
			return nil
		}
		n := ref.Name().String()
		if prefix := "refs/tags/" + remote + "/"; strings.HasPrefix(n, prefix) {
			tagCommits[n[len(prefix):]] = ref.Hash()
		}
		return nil
	})
	return tagCommits, err
}

func ensureRepo(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return os.MkdirAll(dir, 0755)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		logrus.Infof("Cloning %s to %s", *sourceRepo, dir)
		cmd := exec.Command("git", "clone", *sourceRepo, dir)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to clone %s: %v", *sourceRepo, err)
		}
	}
	return nil
}

func main() {
	flag.Parse()
	err := ensureRepo(*workdir)
	if err != nil {
		logrus.Fatalf("Failed to ensure repo: %v", err)
	}
	r, err := gogit.PlainOpen(*workdir)
	if err != nil {
		logrus.Fatalf("Failed to open repo at %s: %v", *workdir, err)
	}

	// set remote
	for _, remote := range []struct{ name, url string }{
		{sourceRemote, *sourceRepo},
		{targetRemote, *targetRepo},
	} {
		if remote.url == "" {
			logrus.Fatalf("Remote %s URL is empty", remote.name)
		}
		rm, _ := r.Remote(remote.name)
		if rm != nil && rm.Config().URLs[0] != remote.url {
			logrus.Infof("Deleting invalid remote %s", remote.name)
			err = r.DeleteRemote(remote.name)
			if err != nil {
				logrus.Fatalf("Failed to delete remote %s: %v", remote.name, err)
			}
			rm = nil
		}
		if rm == nil {
			_, err = r.CreateRemote(&config.RemoteConfig{
				Name: remote.name,
				URLs: []string{remote.url},
			})
			if err != nil {
				logrus.Fatalf("Failed to set remote %s %s: %v", remote.name, remote.url, err)
			}
		}
		err = r.Fetch(&gogit.FetchOptions{
			RemoteName: remote.name,
			RefSpecs: []config.RefSpec{
				config.RefSpec("refs/tags/*:refs/tags/" + remote.name + "/*"),
			},
		})
		if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			logrus.Fatalf("Failed to fetch %s: %v", remote.name, err)
		}
	}

	sourceTagCommits, err := remoteTags(r, sourceRemote)
	if err != nil {
		logrus.Fatalf("Failed to iterate through %s tags: %v", sourceRemote, err)
	}
	for name, kh := range sourceTagCommits {
		// ignore non-annotated tags
		// this logic is from publishing-bot
		_, err := r.TagObject(kh)
		if err != nil {
			delete(sourceTagCommits, name)
			continue
		}
		// after https://github.com/kubernetes/kubernetes/commit/0737e92da613568379d29db8ec18f2ecc240898d
		if semver.Compare(name, "v1.26.0") < 0 {
			delete(sourceTagCommits, name)
			continue
		}
	}

	targetTagCommits, err := remoteTags(r, targetRemote)
	if err != nil {
		logrus.Fatalf("Failed to iterate through %s tags: %v", targetRemote, err)
	}
	tagsToCopy := map[string]plumbing.Hash{}
	for name := range sourceTagCommits {
		if _, ok := targetTagCommits[name+"-mod"]; !ok {
			tagsToCopy[name] = sourceTagCommits[name]
		}
	}
	logrus.Infof("%d tags to copy: %s", len(tagsToCopy), strings.Join(slices.Sorted(maps.Keys(tagsToCopy)), ", "))

	for name, kh := range tagsToCopy {
		err = handleTag(r, name, kh)
		if err != nil {
			logrus.Fatalf("Failed to handle tag %s: %v", name, err)
		}
	}
}

func prepareModFile(fileSystem billy.Filesystem, tag string) error {
	tag = "v0" + strings.TrimPrefix(tag, "v1")
	b, err := os.ReadFile(filepath.Join(fileSystem.Root(), "go.mod"))
	if err != nil {
		return fmt.Errorf("Failed to read go.mod: %v", err)
	}
	modFile, err := modfile.Parse("go.mod", b, nil)
	if err != nil {
		return fmt.Errorf("failed to parse go.mod: %v", err)
	}

	requires := map[string]*modfile.Require{}
	for _, require := range modFile.Require {
		requires[require.Mod.Path] = require
	}
	for _, replace := range modFile.Replace {
		if _, ok := requires[replace.Old.Path]; ok {
			requires[replace.Old.Path].Mod.Version = tag
			modFile.SetRequire(slices.Collect(maps.Values(requires)))
		}
		_ = modFile.DropReplace(replace.Old.Path, replace.Old.Version)
	}

	modFile.Cleanup()
	out, err := modFile.Format()
	if err != nil {
		return fmt.Errorf("failed to format go.mod: %v", err)
	}

	f, err := fileSystem.OpenFile("go.mod", os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open go.mod: %v", err)
	}
	defer f.Close()
	_, err = f.Write(out)
	if err != nil {
		return fmt.Errorf("failed to write go.mod: %v", err)
	}
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = fileSystem.Root()
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("failed to tidy go.mod: %v", err)
	}
	return nil
}

func handleTag(r *gogit.Repository, name string, kh plumbing.Hash) error {
	logrus.Infof("Handling tag %s", name)

	tag, err := r.TagObject(kh)
	if err != nil {
		return fmt.Errorf("failed to get tag %s: %v", name, err)
	}
	commit, err := tag.Commit()
	if err != nil {
		return fmt.Errorf("failed to get commit %s: %v", tag.Target, err)
	}

	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %v", err)
	}
	err = w.Checkout(&gogit.CheckoutOptions{
		Hash: kh,
	})
	if err != nil {
		return fmt.Errorf("failed to checkout: %v", err)
	}

	err = prepareModFile(w.Filesystem, name)
	if err != nil {
		return fmt.Errorf("failed to prepare mod file: %v", err)
	}
	_, err = w.Add("go.mod")
	if err != nil {
		return fmt.Errorf("failed to add go.mod: %v", err)
	}
	_, err = w.Add("go.sum")
	if err != nil {
		return fmt.Errorf("failed to add go.mod: %v", err)
	}

	tagName := name + "-mod"
	newCommit, err := w.Commit("Prepare "+tagName, &gogit.CommitOptions{
		Author: &object.Signature{
			Name: "kksyncer",
			When: commit.Author.When,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to commit go.mod: %v", err)
	}
	_, err = r.CreateTag(tagName, newCommit, nil)
	if err != nil {
		return fmt.Errorf("failed to create tag %s: %v", name, err)
	}
	err = r.Push(&gogit.PushOptions{
		RemoteName: targetRemote,
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/tags/" + tagName + ":refs/tags/" + tagName),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to push tag %s: %v", tagName, err)
	}
	return nil
}
