# sds-object — дизайн модуля

> Статус: дизайн (черновик). Этот документ описывает исходную целевую
> архитектуру и контракт CRD.
>
> **Обновление модели бакетов (актуальная реализация).** Описанный ниже
> namespaced `ObjectBucket` заменён на три ресурса:
> - `ObjectStorageBucket` (**cluster-scoped**) — только объявление бакета, без
>   учётных данных;
> - `ObjectStorageBucketAccess` (namespaced) — запрос доступа из namespace:
>   контроллер выдаёт отдельную пару ключей на каждый access, пишет `Secret`
>   с учётками (owned by access) и поддерживает ротацию ключей по аннотации
>   `storage.deckhouse.io/rotate`;
> - `ObjectStorageBucketPolicy` (cluster-scoped) — из каких namespace разрешён
>   доступ к бакету (deny-by-default; списки имён и RE2-паттерны).
>
> Также: у `ObjectStorageCluster` появился `spec.reclaimPolicy`
> (`Retain`/`Delete`, по умолчанию `Retain`), защищающий данные от случайного
> удаления (для Heavy — `preservePoolsOnDelete`); SeaweedFS использует общий
> PostgreSQL только в `HighRedundancy`, иначе встроенный leveldb. Актуальный
> контракт — в `crds/` и `docs/`.

## 1. Назначение

`sds-object` управляет **S3-совместимым объектным хранилищем** в кластере
Deckhouse. Платформа noops: пользователь декларирует *что* ему нужно, модуль
сам разворачивает и сопровождает бэкенд. Низкоуровневые ручки бэкендов наружу
не выносятся — по аналогии с тем, как `sds-elastic` прячет настройки Ceph за
интент-абстракциями (`replication: ConsistencyAndAvailability` и т. п.).

Модуль предоставляет **два CRD**:

| CRD | Scope | Назначение | Аналог в sds-elastic |
|-----|-------|------------|----------------------|
| `ObjectStorageCluster` (osc) | Cluster | Развернуть кластер объектного хранилища одного из 4 типов | `ElasticCluster` |
| `ObjectBucket` (ob) | **Namespaced** | Создать бакет + S3-учётку (Secret) рядом с приложением | `ElasticStorageClass` |

`ObjectBucket` namespaced — это ключевое отличие от sds-elastic и осознанный
выбор под noops-самообслуживание: команда создаёт бакет в своём namespace, а
сгенерированные ключи доступа кладутся в `Secret` в том же namespace. RBAC
естественно разграничивается по namespace.

Существующий placeholder `ObjectStorageClass` (cluster-scoped) при реализации
**удаляется** и заменяется на два CRD выше (нужно обновить хуки удаления
финализаторов, вебхук-конфигурацию и RBAC — см. §8).

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

## 3. CRD `ObjectStorageCluster` (cluster-scoped)

### 3.1 Spec

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageCluster
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

## 4. CRD `ObjectBucket` (namespaced)

### 4.1 Spec

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectBucket
metadata:
  name: my-bucket
  namespace: my-app
spec:
  clusterRef: system            # имя ObjectStorageCluster — REQUIRED, immutable
  bucketName: ""                # имя бакета в S3; по умолчанию = metadata.name; immutable
  accessPolicy: Private         # Private | PublicRead   (default Private)
  quota:
    maxSize: 10Gi               # optional, лимит ёмкости бакета
    maxObjects: 0               # optional, 0 = без лимита
```

### 4.2 Status

```yaml
status:
  observedGeneration: 1
  phase: Ready                  # Pending | InProgress | Ready | Error
  endpoint: http://system.d8-sds-object.svc.cluster.local
  bucketName: my-bucket
  # Secret в ТОМ ЖЕ namespace с учёткой доступа к бакету
  secretRef:
    name: my-bucket-credentials
  conditions:                   # BucketReady | CredentialsReady | Ready
    - type: Ready
```

### 4.3 Контракт Secret (стандартизированный для noops)

Контроллер создаёт в namespace бакета `Secret` (owned by `ObjectBucket`) с
ключами, удобными для прямого `envFrom` / монтирования:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-bucket-credentials
  namespace: my-app
type: Opaque
stringData:
  S3_ENDPOINT: http://system.d8-sds-object.svc.cluster.local
  S3_REGION: us-east-1
  S3_BUCKET: my-bucket
  AWS_ACCESS_KEY_ID: <generated>
  AWS_SECRET_ACCESS_KEY: <generated>
```

### 4.4 Валидация

- `spec.clusterRef` — immutable; ссылка должна существовать (иначе `phase=Pending`
  с условием, пока кластер не появится / не станет Ready).
- `spec.bucketName` (или `metadata.name`) — соответствует правилам именования
  S3-бакета; immutable.
- Бакет уникален в рамках `(clusterRef, bucketName)`.

### 4.5 Printer columns

```
Cluster   .spec.clusterRef
Bucket    .status.bucketName
Phase     .status.phase
Secret    .status.secretRef.name
Ready     .status.conditions[?(@.type=="Ready")].status
Age       .metadata.creationTimestamp
```

## 5. Архитектура контроллера

Один контроллер (controller-runtime), два реконсайлера + слой бэкенд-драйверов
с общим интерфейсом:

```
ObjectStorageClusterReconciler ──┐
                                 ├─→ backend.Driver (Garage | SeaweedFS | CephRGW)
ObjectBucketReconciler ──────────┘
```

Интерфейс драйвера (примерно):

```go
type Driver interface {
    EnsureCluster(ctx, *ObjectStorageCluster) (Endpoint, error)  // развернуть/обновить dataplane
    ClusterStatus(ctx, *ObjectStorageCluster) (BackendStatus, error)
    EnsureBucket(ctx, cluster, *ObjectBucket) (BucketCreds, error)  // создать бакет + ключи
    DeleteBucket(ctx, cluster, *ObjectBucket) error
}
```

### 5.1 Реконсиляция кластера

- **System (Garage):** DaemonSet с `nodeSelector: node-role.kubernetes.io/control-plane`
  и tolerations на мастера; том `hostPath` (напр. `/var/lib/deckhouse/sds-object/garage`);
  Service (S3 API) + headless для членства; admin API для layout; replication_factor
  по числу мастеров. Админ-ключ → Secret в namespace модуля.
- **Lightweight (Garage):** StatefulSet с `volumeClaimTemplates` (`storage.class`,
  `storage.size`); в остальном как System.
- **Full (SeaweedFS):** master(ы) (raft 1/3), volume-servers (StatefulSet+PVC),
  filer со встроенным S3-gateway; Service на S3-эндпойнт; репликация/EC из `redundancy`.
- **Heavy (Ceph RGW):** контроллер создаёт `CephObjectStore` в namespace
  `sds-elastic` (`d8-sds-elastic`), привязанный к CephCluster из `elasticClusterRef`;
  RGW-поды и Service поднимает Rook; пулы метаданных/данных — из `redundancy`.

### 5.2 Реконсиляция бакета

1. Найти `ObjectStorageCluster` по `clusterRef`, дождаться `Ready`.
2. Через admin API бэкенда создать бакет (если нет), применить policy/quota.
3. Создать access key/secret key с правами на этот бакет.
4. Записать `Secret` в namespace бакета (§4.3), проставить `status.secretRef`.
5. Финализатор: при удалении `ObjectBucket` — удалить ключ и (опционально по
   reclaim-политике) бакет.

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
```

Вся специфика кластеров и бакетов — только через CRD.

## 7. Зависимости

- **Heavy** требует установленного и настроенного `sds-elastic` (Rook Ceph) с
  готовым `ElasticCluster`. Жёсткую зависимость в `module.yaml` ставить не нужно
  — она нужна только для типа Heavy; проверяется в рантайме реконсайлером
  (условие `Error`/`Pending`, если sds-elastic недоступен).
- System/Lightweight/Full самодостаточны (Garage/SeaweedFS вендорятся в образы
  модуля).

## 8. Что меняется относительно текущего скелета

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

- Reclaim-политика бакета при удалении CR (удалять данные или только ключ?) —
  предлагается поле `spec.reclaimPolicy: Delete|Retain` (default `Retain`).
- Мульти-регион / геораспределение Garage — вне первой версии.
- Экспонирование S3-эндпойнта наружу кластера (Ingress) — вне первой версии,
  пока только внутрикластерный Service.
- Нужен ли отдельный системный кластер по умолчанию (авто-провижн) — решено:
  только через явный CR `type: System`.
