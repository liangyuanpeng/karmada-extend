
[English](./README.md)

# karmada-extend

:warning: 该项目是一个实验项目 :warning: 

最终目标是规范karmada在有状态应用operator的场景应用.

Karmada-extend 正在探索将 Operator 和 Karmada 结合起来以实现有状态应用程序。 karmada-extend 目前实现的功能如下：
- [x] 匹配所有 Karmada ResourceBinding 资源，并将调度结果以 Configmap 的形式分发到所有匹配的成员集群.

NOTE: 作为快速探索，我将README设置为中文而不是英文.

# 部署

## 前提条件

- kubernetes 集群
- 部署好 karmada

## 部署 karmada-extend

```shell
kubectl apply -f https://raw.githubusercontent.com/liangyuanpeng/karmada-extend/main/deploy/karmada-extend.yaml
```

## 部署你的有状态 CRD 资源以及 karmada 自定义资源解释器(有必要的话)

目前提供了 Xline 相关资源作为示例  https://github.com/liangyuanpeng/karmada-extend/tree/main/deploy

