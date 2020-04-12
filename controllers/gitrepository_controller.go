/*
Copyright 2020 The Flux CD contributors.

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

package controllers

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sourcev1 "github.com/fluxcd/source-controller/api/v1alpha1"
)

// GitRepositoryReconciler reconciles a GitRepository object
type GitRepositoryReconciler struct {
	client.Client
	Log     logr.Logger
	Scheme  *runtime.Scheme
	Storage *Storage
	Kind    string
}

// +kubebuilder:rbac:groups=source.fluxcd.io,resources=gitrepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.fluxcd.io,resources=gitrepositories/status,verbs=get;update;patch

func (r *GitRepositoryReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	log := r.Log.WithValues(r.Kind, req.NamespacedName)

	var repo sourcev1.GitRepository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	result := ctrl.Result{RequeueAfter: repo.Spec.Interval.Duration}

	// set initial status
	if reset, status := r.shouldResetStatus(repo); reset {
		log.Info("Initializing repository")
		repo.Status = status
		if err := r.Status().Update(ctx, &repo); err != nil {
			log.Error(err, "unable to update GitRepository status")
			return result, err
		}
	}

	// try to remove old artifacts
	r.gc(repo)

	// try git clone
	readyCondition, artifacts, err := r.sync(repo)
	if err != nil {
		log.Info("Repository sync failed", "error", err.Error())
	} else {
		// update artifacts if commit hash changed
		if repo.Status.Artifact != artifacts {
			timeNew := metav1.Now()
			repo.Status.LastUpdateTime = &timeNew
			repo.Status.Artifact = artifacts
		}
		log.Info("Repository sync succeeded", "msg", readyCondition.Message)
	}

	// update status
	readyCondition.LastTransitionTime = metav1.Now()
	repo.Status.Conditions = []sourcev1.SourceCondition{readyCondition}

	if err := r.Status().Update(ctx, &repo); err != nil {
		log.Error(err, "unable to update GitRepository status")
		return result, err
	}

	// requeue repository
	return result, nil
}

func (r *GitRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.GitRepository{}).
		WithEventFilter(RepositoryChangePredicate{}).
		WithEventFilter(predicate.Funcs{
			DeleteFunc: func(e event.DeleteEvent) bool {
				// delete artifacts
				artifact := r.Storage.ArtifactFor(r.Kind, e.Meta, "dummy")
				if err := r.Storage.RemoveAll(artifact); err != nil {
					r.Log.Error(err, "unable to delete artifacts",
						r.Kind, fmt.Sprintf("%s/%s", e.Meta.GetNamespace(), e.Meta.GetName()))
				} else {
					r.Log.Info("Repository artifacts deleted",
						r.Kind, fmt.Sprintf("%s/%s", e.Meta.GetNamespace(), e.Meta.GetName()))
				}
				return false
			},
		}).
		Complete(r)
}

func (r *GitRepositoryReconciler) sync(repository sourcev1.GitRepository) (sourcev1.SourceCondition, string, error) {
	// set defaults: master branch, no tags fetching, max two commits
	branch := "master"
	tagMode := git.NoTags
	depth := 2

	// determine ref
	refName := plumbing.NewBranchReferenceName(branch)
	if repository.Spec.Reference != nil {
		if repository.Spec.Reference.Branch != "" {
			branch = repository.Spec.Reference.Branch
			refName = plumbing.NewBranchReferenceName(branch)
		}
		if repository.Spec.Reference.Commit != "" {
			depth = 0
		} else {
			if repository.Spec.Reference.Tag != "" {
				refName = plumbing.NewTagReferenceName(repository.Spec.Reference.Tag)
			}
			if repository.Spec.Reference.SemVer != "" {
				tagMode = git.AllTags
			}
		}
	}

	// create tmp dir for SSH known_hosts
	tmpSSH, err := ioutil.TempDir("", repository.Name)
	if err != nil {
		err = fmt.Errorf("tmp dir error: %w", err)
		return NotReadyCondition(sourcev1.StorageOperationFailedReason, err.Error()), "", err
	}
	defer os.RemoveAll(tmpSSH)

	auth, err := r.auth(repository, tmpSSH)
	if err != nil {
		err = fmt.Errorf("auth error: %w", err)
		return NotReadyCondition(sourcev1.AuthenticationFailedReason, err.Error()), "", err
	}

	// create tmp dir for the Git clone
	tmpGit, err := ioutil.TempDir("", repository.Name)
	if err != nil {
		err = fmt.Errorf("tmp dir error: %w", err)
		return NotReadyCondition(sourcev1.StorageOperationFailedReason, err.Error()), "", err
	}
	defer os.RemoveAll(tmpGit)

	// clone to tmp
	repo, err := git.PlainClone(tmpGit, false, &git.CloneOptions{
		URL:               repository.Spec.URL,
		Auth:              auth,
		RemoteName:        "origin",
		ReferenceName:     refName,
		SingleBranch:      true,
		NoCheckout:        false,
		Depth:             depth,
		RecurseSubmodules: 0,
		Progress:          nil,
		Tags:              tagMode,
	})
	if err != nil {
		err = fmt.Errorf("git clone error: %w", err)
		return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
	}

	// checkout commit or tag
	if repository.Spec.Reference != nil {
		if commit := repository.Spec.Reference.Commit; commit != "" {
			w, err := repo.Worktree()
			if err != nil {
				err = fmt.Errorf("git worktree error: %w", err)
				return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
			}

			err = w.Checkout(&git.CheckoutOptions{
				Hash:  plumbing.NewHash(commit),
				Force: true,
			})
			if err != nil {
				err = fmt.Errorf("git checkout %s for %s error: %w", commit, branch, err)
				return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
			}
		} else if exp := repository.Spec.Reference.SemVer; exp != "" {
			rng, err := semver.ParseRange(exp)
			if err != nil {
				err = fmt.Errorf("semver parse range error: %w", err)
				return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
			}

			repoTags, err := repo.Tags()
			if err != nil {
				err = fmt.Errorf("git list tags error: %w", err)
				return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
			}

			tags := make(map[string]string)
			_ = repoTags.ForEach(func(t *plumbing.Reference) error {
				tags[t.Name().Short()] = t.Strings()[1]
				return nil
			})

			svTags := make(map[string]string)
			svers := []semver.Version{}
			for tag, _ := range tags {
				v, _ := semver.ParseTolerant(tag)
				if rng(v) {
					svers = append(svers, v)
					svTags[v.String()] = tag
				}
			}

			if len(svers) > 0 {
				semver.Sort(svers)
				v := svers[len(svers)-1]
				t := svTags[v.String()]
				commit := tags[t]

				w, err := repo.Worktree()
				if err != nil {
					err = fmt.Errorf("git worktree error: %w", err)
					return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
				}

				err = w.Checkout(&git.CheckoutOptions{
					Hash: plumbing.NewHash(commit),
				})
				if err != nil {
					err = fmt.Errorf("git checkout error: %w", err)
					return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
				}
			} else {
				err = fmt.Errorf("no match found for semver: %s", repository.Spec.Reference.SemVer)
				return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
			}
		}
	}

	// read commit hash
	ref, err := repo.Head()
	if err != nil {
		err = fmt.Errorf("git resolve HEAD error: %w", err)
		return NotReadyCondition(sourcev1.GitOperationFailedReason, err.Error()), "", err
	}

	artifact := r.Storage.ArtifactFor(r.Kind, repository.ObjectMeta.GetObjectMeta(),
		fmt.Sprintf("%s.tar.gz", ref.Hash().String()))

	// create artifact dir
	err = r.Storage.MkdirAll(artifact)
	if err != nil {
		err = fmt.Errorf("mkdir dir error: %w", err)
		return NotReadyCondition(sourcev1.StorageOperationFailedReason, err.Error()), "", err
	}

	// acquire lock
	unlock, err := r.Storage.Lock(artifact)
	if err != nil {
		err = fmt.Errorf("unable to acquire lock: %w", err)
		return NotReadyCondition(sourcev1.StorageOperationFailedReason, err.Error()), "", err
	}
	defer unlock()

	// archive artifact
	err = r.Storage.Archive(artifact, tmpGit, "")
	if err != nil {
		err = fmt.Errorf("storage archive error: %w", err)
		return NotReadyCondition(sourcev1.StorageOperationFailedReason, err.Error()), "", err
	}

	// update latest symlink
	err = r.Storage.Symlink(artifact, "latest.tar.gz")
	if err != nil {
		err = fmt.Errorf("storage lock error: %w", err)
		return NotReadyCondition(sourcev1.StorageOperationFailedReason, err.Error()), "", err
	}

	message := fmt.Sprintf("Artifact is available at: %s", artifact.Path)
	return ReadyCondition(sourcev1.GitOperationSucceedReason, message), artifact.URL, nil
}

func (r *GitRepositoryReconciler) shouldResetStatus(repository sourcev1.GitRepository) (bool, sourcev1.GitRepositoryStatus) {
	resetStatus := false
	if repository.Status.Artifact != "" {
		parts := strings.Split(repository.Status.Artifact, "/")
		artifact := r.Storage.ArtifactFor(r.Kind, repository.ObjectMeta.GetObjectMeta(), parts[len(parts)-1])
		if !r.Storage.ArtifactExist(artifact) {
			resetStatus = true
		}
	}

	// set initial status
	if len(repository.Status.Conditions) == 0 || resetStatus {
		resetStatus = true
	}

	return resetStatus, sourcev1.GitRepositoryStatus{
		Conditions: []sourcev1.SourceCondition{
			{
				Type:               sourcev1.ReadyCondition,
				Status:             corev1.ConditionUnknown,
				Reason:             sourcev1.InitializingReason,
				LastTransitionTime: metav1.Now(),
			},
		},
	}
}

func (r *GitRepositoryReconciler) gc(repository sourcev1.GitRepository) {
	if repository.Status.Artifact != "" {
		parts := strings.Split(repository.Status.Artifact, "/")
		artifact := r.Storage.ArtifactFor(r.Kind, repository.ObjectMeta.GetObjectMeta(), parts[len(parts)-1])
		if err := r.Storage.RemoveAllButCurrent(artifact); err != nil {
			r.Log.Info("Artifacts GC failed", "error", err)
		}
	}
}

func (r *GitRepositoryReconciler) auth(repository sourcev1.GitRepository, tmp string) (transport.AuthMethod, error) {
	if repository.Spec.SecretRef == nil {
		return nil, nil
	}

	name := types.NamespacedName{
		Namespace: repository.GetNamespace(),
		Name:      repository.Spec.SecretRef.Name,
	}

	var secret corev1.Secret
	err := r.Client.Get(context.TODO(), name, &secret)
	if err != nil {
		return nil, err
	}

	credentials := secret.Data

	// HTTP auth
	if strings.HasPrefix(repository.Spec.URL, "http") {
		auth := &http.BasicAuth{}
		if username, ok := credentials["username"]; ok {
			auth.Username = string(username)
		}
		if password, ok := credentials["password"]; ok {
			auth.Password = string(password)
		}

		if auth.Username == "" || auth.Password == "" {
			return nil, fmt.Errorf("invalid '%s' secret data: required fields username and password",
				repository.Spec.SecretRef.Name)
		}

		return auth, nil
	}

	// SSH auth
	if strings.HasPrefix(repository.Spec.URL, "ssh") {
		var privateKey []byte
		if identity, ok := credentials["identity"]; ok {
			privateKey = identity
		} else {
			return nil, fmt.Errorf("invalid '%s' secret data: required field identity", repository.Spec.SecretRef.Name)
		}

		pk, err := ssh.NewPublicKeys("git", privateKey, "")
		if err != nil {
			return nil, err
		}

		known_hosts := filepath.Join(tmp, "known_hosts")
		if kh, ok := credentials["known_hosts"]; ok {
			if err := ioutil.WriteFile(filepath.Join(tmp, "known_hosts"), kh, 0644); err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("invalid '%s' secret data: required field known_hosts", repository.Spec.SecretRef.Name)
		}

		callback, err := ssh.NewKnownHostsCallback(known_hosts)
		if err != nil {
			return nil, err
		}
		pk.HostKeyCallback = callback

		return pk, nil
	}

	return nil, nil
}