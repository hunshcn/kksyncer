# kksyncer

A simple project to make a easy way to import k8s.io/kubernetes with no `k8s.io/*` replace directive.

It may help you if you want to use [Custom Scheduler Plugin](https://github.com/kubernetes/enhancements/blob/master/keps/sig-scheduling/624-scheduling-framework/README.md#custom-scheduler-plugins-out-of-tree).

(This is repo is only a tool to make a mod-fixed tag of `k8s.io/kubernetes`)

## How to use

```go.mod
// in go.mod 
require (
    k8s.io/kubernetes v1.29.0
)

replace (
    k8s.io/kubernetes => github.com/hunshcn/kubernetes v1.29.0-mod
)
```

no longer need to replace `k8s.io/*`.

**Notice: Only version >= 1.26.0 will be provided.**

## more about this project

see https://github.com/kubernetes/kubernetes/issues/126261
