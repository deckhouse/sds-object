---
title: "Использование"
description: "Развёртывание объектного хранилища через sds-object: включение модуля, создание cluster-scoped ObjectStorageBucket, выдача доступа namespace через ObjectStorageBucketPolicy и получение учётных данных через ObjectStorageBucketAccess."
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

`ObjectStorageBucket` — **cluster-scoped**-ресурс: объявляет бакет в кластере, но не содержит учётных данных:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageBucket
metadata:
  name: app-data
spec:
  clusterRef: shared
  # bucketName по умолчанию равен metadata.name
  accessPolicy: Private
  reclaimPolicy: Retain
```

```shell
d8 k get objectstoragebucket app-data
# NAME       CLUSTER   BUCKET     PHASE   READY   AGE
# app-data   shared    app-data   Ready   True    30s
```

## Выдача доступа namespace

Доступ к бакету работает по принципу **deny-by-default**: namespace может получить учётные данные только если существует `ObjectStorageBucketPolicy` для этого бакета, совпадающая с namespace. Namespace выбираются точными именами `names` и/или RE2-паттернами `patterns`:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageBucketPolicy
metadata:
  name: app-data-teams
spec:
  bucketRef: app-data
  allowedNamespaces:
    names:
      - my-app
    patterns:
      - "team-.*"
```

## Запрос учётных данных

Каждый потребляющий namespace создаёт `ObjectStorageBucketAccess`. Контроллер генерирует отдельную пару access key / secret key с доступом к бакету и пишет `Secret` (по умолчанию `<access>-s3-credentials`) в том же namespace:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageBucketAccess
metadata:
  name: app-data
  namespace: my-app
spec:
  bucketRef: app-data
  permission: ReadWrite   # или ReadOnly
```

```shell
d8 k -n my-app get objectstoragebucketaccess app-data
# NAME       BUCKET     PHASE   SECRET                    READY   AGE
# app-data   app-data   Ready   app-data-s3-credentials   True    20s
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

## Ротация учётных данных

Чтобы сменить access key у `ObjectStorageBucketAccess`, установите или измените аннотацию `storage.deckhouse.io/rotate`. Контроллер выпустит новую пару ключей, обновит `Secret` и отзовёт предыдущий ключ:

```shell
d8 k -n my-app annotate objectstoragebucketaccess app-data \
  storage.deckhouse.io/rotate="$(date +%s)" --overwrite
```

## Политика высвобождения

- Бакет `reclaimPolicy: Retain` (по умолчанию) — при удалении `ObjectStorageBucket` бакет и объекты сохраняются; `Delete` — удаляются.
- Удаление `ObjectStorageBucketAccess` всегда отзывает его ключ доступа и удаляет его `Secret` (данные бакета не затрагиваются).
- Кластер `reclaimPolicy: Retain` (по умолчанию) — при удалении `ObjectStorageCluster` данные сохраняются (для `Heavy` — пулы Ceph RGW; для профилей на PVC — сами PVC). `Delete` — данные уничтожаются.

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
Профиль `Heavy` разворачивает data plane Ceph RGW (Rook CephObjectStore на указанном кластере sds-elastic). Бакеты и доступ работают так же, как у остальных профилей: `ObjectStorageBucket` создаёт владельца-бакета Rook `CephObjectStoreUser` и сам бакет, а каждый `ObjectStorageBucketAccess` получает собственного `CephObjectStoreUser`, которому доступ к бакету выдаётся через bucket policy.
{{< /alert >}}
