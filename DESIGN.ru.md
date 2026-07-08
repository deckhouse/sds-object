# sds-object — дизайн модуля

> Статус: дизайн (черновик). Этот документ описывает исходную целевую
> архитектуру и контракт CRD.
>
> **Обновление модели бакетов (актуальная реализация, COSI-выравнивание).**
> Модель приведена к COSI (`Bucket` + `BucketClaim`), CRD `ObjectStorageCluster`
> переименован в `ObjectStore`. Текущий набор из пяти ресурсов:
> - `ObjectStore` (**cluster-scoped**) — разворачивает data plane хранилища
>   (вне модели COSI); ранее назывался `ObjectStorageCluster`;
> - `Bucket` (**cluster-scoped**) — бакет-бэкенд: либо объявлен администратором
>   (Shared), либо создан контроллером под greenfield-`BucketClaim` (origin в
>   лейбле `storage.deckhouse.io/bucket-origin`);
> - `BucketClaim` (**namespaced**) — заявка на бакет: greenfield (создаёт
>   собственный приватный `Bucket`) или brownfield (`spec.existingBucketName` —
>   привязка Shared-бакета, гейтится `BucketPolicy`, deny-by-default);
> - `BucketAccess` (namespaced) — запрос учётных данных к `BucketClaim` в том же
>   namespace: контроллер выдаёт отдельную пару ключей, пишет `Secret`
>   (owned by access) и поддерживает ротацию по аннотации
>   `storage.deckhouse.io/rotate`;
> - `BucketPolicy` (cluster-scoped) — из каких namespace разрешена привязка
>   Shared-бакета через brownfield-`BucketClaim` (deny-by-default; имена + RE2).
>
> Также: у `ObjectStore` есть `spec.reclaimPolicy` (`Retain`/`Delete`, по
> умолчанию `Retain`); SeaweedFS использует общий PostgreSQL только в
> `HighRedundancy`, иначе встроенный leveldb. Разделы §3–§4 ниже описывают
> исходные имена полей (`clusterRef`, `bucketRef`); **актуальный контракт полей
> и имён — в `crds/` и `docs/`** (`objectStoreRef`, `bucketClaimName` и т. д.).

## 1. Назначение

`sds-object` управляет **S3-совместимым объектным хранилищем** в кластере
Deckhouse. Платформа noops: пользователь декларирует *что* ему нужно, модуль
сам разворачивает и сопровождает бэкенд. Низкоуровневые ручки бэкендов наружу
не выносятся — по аналогии с тем, как `sds-elastic` прячет настройки Ceph за
интент-абстракциями (`replication: ConsistencyAndAvailability` и т. п.).

Модуль предоставляет **пять CRD** (COSI-выравнивание):

| CRD | Scope | Назначение |
|-----|-------|------------|
| `ObjectStore` (ostore) | Cluster | Развернуть объектное хранилище одного из 4 типов (data plane; вне COSI) |
| `Bucket` (bkt) | Cluster | Бакет-бэкенд: Shared (объявлен админом) или созданный под `BucketClaim` |
| `BucketClaim` (bc) | **Namespaced** | Заявка на бакет: greenfield (свой приватный бакет) или brownfield (привязка Shared-бакета, гейтится политикой) |
| `BucketAccess` (ba) | **Namespaced** | Запросить учётные данные к `BucketClaim` — отдельная пара ключей + `Secret` рядом с приложением; ротация по аннотации |
| `BucketPolicy` (bp) | Cluster | Задать, из каких namespace разрешена привязка Shared-бакета через brownfield-заявку (deny-by-default; имена + RE2-паттерны) |

Greenfield-заявка создаёт собственный приватный бакет (namespace-локальный, имя
под зарезервированным префиксом — не может совпасть с Shared-бакетом). Shared-бакет
— общая (cluster-scoped) платформенная сущность; привязать его из namespace можно
только brownfield-заявкой, гейтящейся `BucketPolicy` (deny-by-default). Это
закрывает риск «захвата» чужого/системного бакета, который был возможен в исходной
namespaced-`ObjectBucket`-модели (см. историческую врезку выше и ADR).

Существующий placeholder `ObjectStorageClass` (cluster-scoped) при реализации
удалён и заменён на CRD выше (обновлены хуки удаления финализаторов,
вебхук-конфигурация и RBAC — см. §8).

## 2. Четыре типа кластера

Один CRD, поле `spec.type` (enum, immutable). Бэкенд скрыт за интент-именем.

| `spec.type` | Бэкенд | Размещение / данные | Сценарий |
|-------------|--------|---------------------|----------|
| `System` | Garage | DaemonSet на control-plane, `hostPath` | Системные нужды платформы (backup, registry, loki, …). Минимум зависимостей, не требует внешнего стораджа. |
| `Lightweight` | Garage | StatefulSet, PVC на StorageClass | Лёгкое хранилище для прикладных нужд небольшого объёма. |
| `Full` | SeaweedFS | StatefulSet (master/volume/filer+s3), PVC | Полноценное масштабируемое хранилище с репликацией/EC. |
| `Heavy` | sds-elastic (Ceph RGW) | RADOS Gateway поверх существующего CephCluster | Тяжёлое хранилище, переиспользует ёмкость и отказоустойчивость Ceph. |

Почему Garage для System и Lightweight: лёгкий S3-сервер на Rust, минимум
компонентов, умеет hostPath/локальные диски, простой admin API для управления
бакетами и ключами. Идеален для «коробочного» системного стораджа на мастерах.

Почему SeaweedFS для Full: распределённое хранилище с master/volume/filer,
S3-gateway, репликацией и erasure coding — масштабируется заметно лучше Garage.

Почему Ceph RGW для Heavy: при наличии `sds-elastic` (Rook Ceph) объектное
хранилище — это `CephObjectStore` поверх уже развёрнутого кластера. Никакого
отдельного дата-плейна, ёмкость и HA берутся у Ceph.

## 3. CRD `ObjectStore` (cluster-scoped)

### 3.1 Spec

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStore
metadata:
  name: system            # DNS-1123 label, <= 30 символов (префикс производных ресурсов)
spec:
  type: System            # System | Lightweight | Full | Heavy — REQUIRED, immutable

  # Ёмкость и сторадж. Игнорируется для type=Heavy (ёмкость у Ceph).
  storage:
    size: 100Gi           # суммарная полезная ёмкость (для System — на узел)
    class: localpath      # имя StorageClass для PVC; REQUIRED для Lightweight/Full,
                          # игнорируется для System (hostPath) и Heavy (Ceph)

  # Размещение dataplane. Для System перекрывается фиксированным
  # размещением на control-plane (см. ниже), nodeSelector можно не задавать.
  placement:
    nodeSelector: {}
    tolerations: []

  # Интент отказоустойчивости. Маппится в конкретные настройки бэкенда.
  # По умолчанию выбирается исходя из type.
  redundancy: Replicated  # Single | Replicated | HighRedundancy   (optional)

  # Только для type=Heavy: ссылка на ElasticCluster (sds-elastic),
  # поверх которого поднимается CephObjectStore.
  elasticClusterRef: ""   # REQUIRED iff type==Heavy, immutable
```

Минимальные примеры по типам:

```yaml
# 1. Системный кластер (garage на мастерах, hostPath)
spec:
  type: System            # всё остальное — дефолты
---
# 2. Lightweight (garage + PVC)
spec:
  type: Lightweight
  storage: { size: 50Gi, class: localpath }
---
# 3. Full (seaweedfs)
spec:
  type: Full
  storage: { size: 2Ti, class: replicated }
  redundancy: HighRedundancy
---
# 4. Heavy (Ceph RGW поверх sds-elastic)
spec:
  type: Heavy
  elasticClusterRef: main
```

### 3.2 Семантика `redundancy`

| Значение | Garage | SeaweedFS | Ceph RGW |
|----------|--------|-----------|----------|
| `Single` | replication_factor=1 | replication=000 | пул size=1 |
| `Replicated` (default) | replication_factor=3* | replication=010/020 | пул size=3, min_size=2 |
| `HighRedundancy` | replication_factor=3 + зоны | EC (k+m) | пул HighRedundancy / EC |

\* Для `System` фактор ограничен числом control-plane узлов (1 или 3).

### 3.3 Status

```yaml
status:
  observedGeneration: 3
  phase: Ready                       # Pending | InProgress | Ready | Error
  backend:
    type: Garage                     # Garage | SeaweedFS | CephRGW
    version: "v1.0.1"
  endpoint:
    internal: http://system.d8-sds-object.svc.cluster.local
    region: us-east-1                # дефолтный регион S3
  capacity:
    total: 300Gi
    used: 42Gi
    available: 258Gi
    usedPercent: "14.00"
    lastUpdated: "2026-06-27T10:00:00Z"
  # Ссылка на Secret с админ-учёткой бэкенда (в namespace модуля)
  adminSecretRef:
    name: system-admin
  conditions:                        # patchMergeKey: type
    - type: BackendReady             # дата-плейн поднят и здоров
    - type: EndpointReady            # S3-эндпойнт доступен
    - type: Ready                    # агрегирующее
```

### 3.4 Валидация (webhook + x-kubernetes-validations)

- `metadata.name` — DNS-1123 label, `<= 30` символов.
- `spec.type` — immutable.
- `spec.elasticClusterRef` — обязателен и непуст ⇔ `type == Heavy`; immutable.
- `spec.storage.class` — обязателен для `Lightweight`/`Full`.
- `type == System`: размещение принудительно на control-plane, `storage.class`
  игнорируется (hostPath); валидатор предупреждает, если задан.
- Рекомендуется не более одного кластера `type: System` (имя по соглашению
  `system`), т. к. на него ссылаются платформенные модули.

### 3.5 Printer columns

```
Type        .spec.type
Phase       .status.phase
Endpoint    .status.endpoint.internal
Used%       .status.capacity.usedPercent      (priority 1)
Capacity    .status.capacity.total            (priority 1)
Ready       .status.conditions[?(@.type=="Ready")].status
Age         .metadata.creationTimestamp
```

## 4. CRD `Bucket` (cluster-scoped), `BucketAccess` (namespaced), `BucketPolicy` (cluster-scoped)

### 4.1 `Bucket` — объявление бакета (без учётных данных)

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: Bucket
metadata:
  name: my-bucket              # cluster-scoped
spec:
  clusterRef: system           # имя ObjectStore — REQUIRED, immutable
  bucketName: ""               # имя бакета в S3; по умолчанию = metadata.name; immutable
  accessPolicy: Private        # Private | PublicRead   (default Private)
  reclaimPolicy: Retain        # Retain | Delete   (default Retain)
  quota:
    maxSize: 10Gi              # optional, лимит ёмкости бакета
    maxObjects: 0              # optional, 0 = без лимита
```

Status: `phase`, `endpoint`, `bucketName`, `conditions` (`BucketReady`, `Ready`).
Учётных данных бакет не содержит.

### 4.2 `BucketPolicy` — кто допущен к бакету (deny-by-default)

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: BucketPolicy
metadata:
  name: my-bucket-teams        # cluster-scoped
spec:
  bucketRef: my-bucket         # Bucket — REQUIRED, immutable
  allowedNamespaces:
    names: [my-app]            # точные имена namespace
    patterns: ["team-.*"]      # RE2-паттерны (полное совпадение)
```

Без совпадающей политики `BucketAccess` не провижнится (остаётся
`Pending` с condition `DeniedByPolicy`). Несколько политик на один бакет
складываются. Enforcement — в реконсайлере доступа (webhook лишь предупреждает).

### 4.3 `BucketAccess` — доступ и ключи (namespaced, self-service)

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: BucketAccess
metadata:
  name: my-access
  namespace: my-app
  annotations:
    storage.deckhouse.io/rotate: "1"   # смена значения → ротация ключей
spec:
  bucketRef: my-bucket         # cluster-scoped Bucket — REQUIRED, immutable
  permission: ReadWrite        # ReadWrite | ReadOnly   (default ReadWrite)
  secretName: ""               # optional; по умолчанию <access>-s3-credentials
```

Status: `phase`, `endpoint`, `bucketName`, `accessKeyID`, `secretRef`,
`observedRotation`, `lastRotationTime`, `conditions` (`AccessGranted`,
`CredentialsReady`, `Ready`).

### 4.4 Контракт Secret (стандартизированный для noops)

Контроллер создаёт в namespace доступа `Secret` (owned by
`BucketAccess`) с ключами, удобными для прямого `envFrom`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-access-s3-credentials
  namespace: my-app
type: Opaque
stringData:
  S3_ENDPOINT: http://system.d8-sds-object.svc.cluster.local
  S3_REGION: us-east-1
  S3_BUCKET: my-bucket
  AWS_ACCESS_KEY_ID: <generated per access>
  AWS_SECRET_ACCESS_KEY: <generated per access>
```

Каждый access получает **свою** пару ключей: независимый отзыв (удаление access)
и ротация (аннотация `storage.deckhouse.io/rotate` → новая пара, обновление
`Secret`, отзыв старого ключа).

### 4.5 Валидация

- `spec.clusterRef`/`spec.bucketRef` — immutable; ссылки должны существовать
  (иначе `Pending`, пока не появятся / не станут Ready).
- `spec.bucketName` (или `metadata.name`) — правила именования S3; immutable.
  Бакет уникален в рамках `(clusterRef, bucketName)`.
- `BucketPolicy.spec.allowedNamespaces.patterns` — валидные RE2
  (webhook отклоняет некомпилируемые); доступ deny-by-default.

## 5. Архитектура контроллера

Один контроллер (controller-runtime), реконсайлеры на каждый CRD + слой
бэкенд-драйверов с общим интерфейсом:

```
ObjectStoreReconciler       ──┐
BucketReconciler         ─┼─→ backend.Driver (Garage | SeaweedFS | CephRGW)
BucketAccessReconciler   ─┤
BucketPolicyReconciler   ──┘   (валидирует политику; enforcement — в Access-реконсайлере)
```

Интерфейс драйвера (бакет и доступ разделены):

```go
type Driver interface {
    EnsureCluster(ctx, *ObjectStore) (ClusterState, error)  // развернуть/обновить dataplane
    DeleteCluster(ctx, *ObjectStore) error                  // с учётом reclaimPolicy
    EnsureBucket(ctx, cluster, *Bucket) (BucketState, error)  // только бакет, без ключей
    DeleteBucket(ctx, cluster, *Bucket) error
    EnsureAccess(ctx, cluster, bucket, *BucketAccess, mintFresh bool) (AccessState, error)  // ключи per-access
    DeleteAccess(ctx, cluster, bucket, *BucketAccess) error
}
```

### 5.1 Реконсиляция кластера

- **System (Garage):** DaemonSet с `nodeSelector: node-role.kubernetes.io/control-plane`
  и tolerations на мастера; том `hostPath` (напр. `/var/lib/deckhouse/sds-object/garage`);
  Service (S3 API) + headless для членства; admin API для layout. `replication_factor`
  ограничивается числом control-plane узлов (нечётное, floor 1) — иначе на 1 мастере
  кластер завис бы в degraded. Админ-ключ → Secret в namespace модуля.
- **Lightweight (Garage):** StatefulSet с `volumeClaimTemplates` (`storage.class`,
  `storage.size`); число реплик = `replication_factor`, фактор достижим по построению.
- **Full (SeaweedFS):** master(ы) (raft 1/3), volume-servers (StatefulSet+PVC),
  filer со встроенным S3-gateway; Service на S3-эндпойнт; репликация/EC из `redundancy`.
  Метаданные filer: встроенный leveldb (Single/Replicated, один filer, без внешних
  зависимостей) или общий PostgreSQL из `managed-postgres` (HighRedundancy, multi-filer HA).
- **Heavy (Ceph RGW):** контроллер создаёт `CephObjectStore` в namespace
  `sds-elastic` (`d8-sds-elastic`), привязанный к CephCluster из `elasticClusterRef`;
  RGW-поды и Service поднимает Rook; пулы метаданных/данных — из `redundancy`.
  `preservePoolsOnDelete` завязан на `reclaimPolicy` кластера (Retain → пулы
  сохраняются).

### 5.2 Реконсиляция бакета и доступа

Бакет (`Bucket`):

1. Найти `ObjectStore` по `clusterRef`, дождаться `Ready`.
2. Через admin/S3 API бэкенда создать бакет (если нет), применить policy/quota.
3. Финализатор: при удалении — удалить бакет только при `reclaimPolicy: Delete`.

Доступ (`BucketAccess`):

1. Найти бакет по `bucketRef`, дождаться `Ready`; проверить, что namespace
   разрешён `BucketPolicy` (deny-by-default). При отзыве политики у
   уже выданного доступа ключ **отзывается**, `Secret` удаляется.
2. Выдать отдельную пару ключей (Garage key / SeaweedFS identity / Ceph RGW user
   + bucket policy) с правами из `permission`; при смене аннотации ротации —
   новая пара + отзыв старого ключа.
3. Записать `Secret` в namespace доступа (§4.4), проставить `status.secretRef`.
4. Финализатор: при удалении `BucketAccess` — отозвать ключ.

Системное хранилище: модуль по умолчанию (`sdsObject.systemBucket.enabled`,
default true) шипует системный `ObjectStore` (`System`) + `system`-бакет
+ политику на `d8-*`; `redundancy` кластера следует режиму HA
(`helm_lib_is_ha_to_value`: HA → Replicated, иначе Single).

Маппинг admin-операций по бэкендам: Garage Admin API; SeaweedFS S3/filer API;
Ceph RGW admin ops (или `CephObjectStoreUser` + bucket через S3 от его имени).

## 6. Настройки модуля (openapi/config-values.yaml)

Только политики оператора, никаких per-cluster ручек (как у sds-elastic):

```yaml
logLevel: INFO
controller:
  resourcesManagement: { mode: VPA, ... }    # ресурсы контроллера
nodeSelector: {}                              # размещение служебных подов модуля
tolerations: []
systemBucket:
  enabled: true                               # шипать системный кластер+бакет (default true)
```

Вся специфика кластеров и бакетов — только через CRD.

## 7. Зависимости

- **Heavy** требует установленного и настроенного `sds-elastic` (Rook Ceph) с
  готовым `ElasticCluster`. Жёсткую зависимость в `module.yaml` ставить не нужно
  — она нужна только для типа Heavy; проверяется в рантайме реконсайлером
  (условие `Error`/`Pending`, если sds-elastic недоступен).
- System/Lightweight/Full самодостаточны (Garage/SeaweedFS вендорятся в образы
  модуля). Исключение: `Full` в режиме `HighRedundancy` требует модуля
  `managed-postgres` (общий слой метаданных filer); в остальных режимах — нет.

## 8. Что меняется относительно текущего скелета

> **Неактуально (исторический раздел).** Скелет давно заменён рабочей
> реализацией: оба CRD, три драйвера, оба реконсайлера, вебхуки и хуки готовы,
> а модель бакетов переработана (см. врезку в начале документа и `crds/`).
> Раздел оставлен для истории миграции с placeholder-CRD `ObjectStorageClass`.

1. `crds/objectstorageclass.yaml` → удалить; добавить
   `crds/objectstoragecluster.yaml` и `crds/objectbucket.yaml`.
2. `api/v1alpha1/object_storage_class.go` → заменить на
   `object_storage_cluster.go` и `object_bucket.go` (+ enum-константы типов,
   redundancy, фаз, conditions).
3. Хуки `030-remove-finalizers-on-module-delete` и `consts/consts.go`:
   обновить `CRGVKsForFinalizerRemoval` (две GVK; `ObjectBucket` namespaced=true),
   `WebhookConfigurationsToDelete`, `AllowedProvisioners`.
4. Вебхуки: заменить `/osc-validate` на валидаторы
   `/objectstoragecluster-validate` (cluster) и `/objectbucket-validate`
   (namespaced); правила immutable/обязательности из §3.4 и §4.4.
5. RBAC (`templates/rbacv2/manage/`): edit/view на оба новых CRD; учесть, что
   `ObjectBucket` namespaced (нужны namespaced-роли для self-service команд).
6. Образы werf: добавить образы дата-плейна (garage, seaweedfs) и/или их
   вендоринг; для Heavy — только интеграция с Rook, отдельный образ не нужен.

## 9. Открытые вопросы

- Reclaim-политика (удалять данные или сохранять) — **реализована**:
  `reclaimPolicy: Retain|Delete` (default `Retain`) на бакете и на кластере
  (для Heavy `Retain` сохраняет пулы Ceph). Вопрос закрыт.
- Owner-тег бэкенд-бакета: `EnsureBucket` пока переиспользует уже существующий
  бакет по имени без проверки владельца — остаточный вопрос изоляции (см. ADR).
- Мульти-регион / геораспределение Garage — вне первой версии.
- Экспонирование S3-эндпойнта наружу кластера (Ingress) — вне первой версии,
  пока только внутрикластерный Service.
- Нужен ли отдельный системный кластер по умолчанию (авто-провижн) — решено:
  только через явный CR `type: System`.
