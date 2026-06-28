---
title: "Использование"
description: "Развёртывание объектного хранилища через sds-object: включение модуля, создание ObjectStorageCluster и ObjectBucket, использование сгенерированного Secret с учётными данными."
weight: 50
---

{{< alert level="warning" >}}
Модуль `sds-object` находится в стадии `Experimental`. Experimental-модули не включены по умолчанию. Перед включением установите `allowExperimentalModules: true` в ModuleConfig `deckhouse`. Сейчас работают только профили `System` и `Lightweight` (Garage).
{{< /alert >}}

## Включение модуля

```shell
d8 k apply -f - <<EOF
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: sds-object
spec:
  enabled: true
  version: 1
EOF
```

## Создание кластера

Кластер `Lightweight` на PVC поверх существующего StorageClass:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageCluster
metadata:
  name: shared
spec:
  type: Lightweight
  storage:
    size: 50Gi
    class: localpath
  redundancy: Replicated
```

Кластер `System` для нужд платформы (Garage на control-plane узлах, `hostPath`; `storage.class` игнорируется):

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageCluster
metadata:
  name: system
spec:
  type: System
```

Отслеживание готовности:

```shell
d8 k get objectstoragecluster
# NAME     TYPE          PHASE   ENDPOINT                                           READY   AGE
# shared   Lightweight   Ready   http://shared-garage.d8-sds-object.svc...:3900     True    3m
```

## Создание бакета

`ObjectBucket` — namespaced-ресурс, создавайте его в namespace приложения:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectBucket
metadata:
  name: app-data
  namespace: my-app
spec:
  clusterRef: shared
  # bucketName по умолчанию равен metadata.name
  accessPolicy: Private
  reclaimPolicy: Retain
```

После перехода в `Ready` контроллер пишет `Secret` с именем `<bucket>-s3-credentials` в том же namespace:

```shell
d8 k -n my-app get objectbucket app-data
# NAME       CLUSTER   BUCKET     PHASE   SECRET                  READY   AGE
# app-data   shared    app-data   Ready   app-data-s3-credentials True    30s
```

## Использование учётных данных

`Secret` содержит стандартные переменные подключения к S3, готовые к монтированию через `envFrom`:

| Ключ | Описание |
|------|----------|
| `S3_ENDPOINT` | Внутрикластерный URL S3-эндпойнта |
| `S3_REGION` | Регион S3 |
| `S3_BUCKET` | Имя бакета |
| `AWS_ACCESS_KEY_ID` | Ключ доступа |
| `AWS_SECRET_ACCESS_KEY` | Секретный ключ |

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: my-app
spec:
  template:
    spec:
      containers:
        - name: app
          image: my-app:latest
          envFrom:
            - secretRef:
                name: app-data-s3-credentials
```

## Политика высвобождения

- `reclaimPolicy: Retain` (по умолчанию) — при удалении `ObjectBucket` ключ доступа удаляется, бакет и объекты сохраняются.
- `reclaimPolicy: Delete` — при удалении `ObjectBucket` удаляются бакет и все его объекты.

## Профиль Heavy

Профиль `Heavy` разворачивает Ceph RADOS Gateway поверх существующего кластера [`sds-elastic`](/modules/sds-elastic/) и выбирается через `spec.elasticClusterRef`:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageCluster
metadata:
  name: heavy
spec:
  type: Heavy
  elasticClusterRef: main
```

{{< alert level="info" >}}
Профиль `Heavy` разворачивает data plane Ceph RGW (Rook CephObjectStore на указанном кластере sds-elastic) и публикует S3-эндпойнт по готовности. Provisioning бакетов/учёток для `Heavy` (`ObjectBucket`) — follow-up.
{{< /alert >}}
