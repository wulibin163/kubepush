# kubepush

Kubepush is actually a controller deploy as daemonset, it list and watch Push resources(a third party resource),
which claims commit a container as image and push it into registry.

### How to use

#### Create ThirdPartyResource
```
metadata:
  name: push.k8s.io
apiVersion: extensions/v1beta1
kind: ThirdPartyResource
description: "Allow user to commit and push a container to image hub"
versions:
- name: v1
```

#### Create kubepush daemonset
```
apiVersion: extensions/v1beta1
kind: DaemonSet
metadata:
  labels:
    app: kubepush
  name: kubepush
  namespace: kube-system
spec:
  template:
    metadata:
      labels:
        app: kubepush
    spec:
      containers:
        - name: kubepush
          image: index.caicloud.io/caicloud/kubepush:latest
          imagePullPolicy: Always
          env:
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
          volumeMounts:
            - name: run
              mountPath: /var/run/docker.sock
      volumes:
        - name: run
          hostPath:
              path: /var/run/docker.sock

```

#### Create Push resource
```
kind: Push
apiVersion: k8s.io/v1
metadata:
  namespace: ns1
  name: name1
  labels:
    kubepush.alpha.kubernetes.io/nodename: i-jgganq
spec:
  podName: master-nginx-i-94uugzhjm
  containerName: master-nginx
  image: cargo.caicloud.io/liangmq/nginx: 1.2
  imagePushSecrets:
  - name: cargo.caicloud.io
status:
  phase: Succeeded
  message: push succeeded
```