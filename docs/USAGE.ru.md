---
title: "Использование"
description: "Развёртывание объектного хранилища через sds-object: включение модуля, создание cluster-scoped Bucket, выдача доступа namespace через BucketClaimPolicy и получение учётных данных через BucketAccess."
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
kind: ObjectStore
metadata:
  name: shared
spec:
  type: Lightweight
  storage:
    sizePerNode: 50Gi
    class: localpath
  redundancy: Standard
```

Кластер `System` для нужд платформы (Garage на control-plane узлах, `hostPath`; `storage.class` игнорируется):

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStore
metadata:
  name: system
spec:
  type: System
```

Отслеживание готовности:

```shell
d8 k get objectstore
# NAME     TYPE          PHASE   ENDPOINT                                           READY   AGE
# shared   Lightweight   Ready   http://shared-garage.d8-sds-object.svc...:3900     True    3m
```

## Объявление Shared-бакета

`Bucket` — **cluster-scoped**-ресурс: администратор объявляет бакет в объектном хранилище, учётных данных он не содержит. Это **Shared**-бакет, предназначенный для потребления из нескольких namespace через заявки с проверкой политики:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: Bucket
metadata:
  name: app-data
spec:
  objectStoreRef: shared
  # bucketName по умолчанию равен metadata.name
  accessPolicy: Private
  reclaimPolicy: Retain
```

```shell
d8 k get bucket app-data
# NAME       OBJECTSTORE   BUCKET     PHASE   READY   AGE
# app-data   shared        app-data   Ready   True    30s
```

## Разрешение namespace привязывать бакет

Привязка Shared-бакета работает по принципу **deny-by-default**: namespace может заявить его, только если существует совпадающая `BucketClaimPolicy` для этого бакета. Namespace выбираются точными именами `names` и/или RE2-паттернами `patterns`:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: BucketClaimPolicy
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

## Заявка на бакет

Каждый потребляющий namespace создаёт `BucketClaim`. Чтобы привязать Shared-бакет выше (brownfield), задайте `existingBucketName`. Чтобы вместо этого создать **новый приватный** бакет (greenfield), опустите `existingBucketName` и задайте `objectStoreRef` — для greenfield-заявок политика не требуется.

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: BucketClaim
metadata:
  name: app-data
  namespace: my-app
spec:
  existingBucketName: app-data   # brownfield: привязать Shared-бакет (гейтится политикой)
  # --- или, для нового приватного бакета, уберите existingBucketName и используйте: ---
  # objectStoreRef: shared
  # accessPolicy: Private
  # reclaimPolicy: Retain
```

```shell
d8 k -n my-app get bucketclaim app-data
# NAME       BUCKET     PHASE   READY   AGE
# app-data   app-data   Ready   True    20s
```

## Запрос учётных данных

Каждый workload создаёт `BucketAccess`, ссылающийся на `BucketClaim` в состоянии Bound в своём namespace. Контроллер генерирует отдельную пару access key / secret key с доступом к привязанному бакету и пишет `Secret` (по умолчанию `<access>-s3-credentials`) в том же namespace:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: BucketAccess
metadata:
  name: app-data
  namespace: my-app
spec:
  bucketClaimName: app-data
  permission: ReadWrite   # или ReadOnly
```

```shell
d8 k -n my-app get bucketaccess app-data
# NAME       CLAIM      PHASE   SECRET                    READY   AGE
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

Чтобы сменить access key у `BucketAccess`, установите или измените аннотацию `storage.deckhouse.io/rotate`. Контроллер выпустит новую пару ключей, обновит `Secret` и отзовёт предыдущий ключ:

```shell
d8 k -n my-app annotate bucketaccess app-data \
  storage.deckhouse.io/rotate="$(date +%s)" --overwrite
```

## Политика высвобождения

- Бакет `reclaimPolicy: Retain` (по умолчанию) — при удалении `Bucket` бакет и объекты сохраняются; `Delete` — удаляются.
- Удаление `BucketAccess` всегда отзывает его ключ доступа и удаляет его `Secret` (данные бакета не затрагиваются).
- Кластер `reclaimPolicy: Retain` (по умолчанию) — при удалении `ObjectStore` данные сохраняются (для `Heavy` — пулы Ceph RGW; для профилей на PVC — сами PVC). `Delete` — данные уничтожаются.

## Профиль Heavy

Профиль `Heavy` разворачивает Ceph RADOS Gateway поверх существующего кластера [`sds-elastic`](/modules/sds-elastic/) и выбирается через `spec.elasticClusterRef`:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStore
metadata:
  name: heavy
spec:
  type: Heavy
  elasticClusterRef: main
```

{{< alert level="info" >}}
Профиль `Heavy` разворачивает data plane Ceph RGW (Rook CephObjectStore на указанном кластере sds-elastic). Бакеты и доступ работают так же, как у остальных профилей: `Bucket` создаёт владельца-бакета Rook `CephObjectStoreUser` и сам бакет, а каждый `BucketAccess` получает собственного `CephObjectStoreUser`, которому доступ к бакету выдаётся через bucket policy.
{{< /alert >}}
