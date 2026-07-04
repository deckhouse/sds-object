---
title: "Модуль sds-object"
description: "Модуль Deckhouse Kubernetes Platform для управления S3-совместимым объектным хранилищем."
weight: 1
---

{{< alert level="warning" >}}
Модуль находится в стадии `Experimental`. API, конфигурация и пользовательские ресурсы могут меняться без предупреждения; не используйте его для production-нагрузок.
{{< /alert >}}

Модуль `sds-object` управляет S3-совместимым объектным хранилищем в кластере Deckhouse Kubernetes Platform. Подход no-ops: вы декларируете *что* вам нужно двумя пользовательскими ресурсами, а модуль сам разворачивает и сопровождает бэкенд. Низкоуровневые настройки бэкенда наружу не выносятся.

## Пользовательские ресурсы

| Ресурс | Область | Назначение |
|--------|---------|------------|
| `ObjectStorageCluster` (`osc`) | Cluster | Разворачивает кластер объектного хранилища одного из четырёх готовых профилей. |
| `ObjectStorageBucket` (`osb`) | Cluster | Объявляет бакет в кластере (без учётных данных). |
| `ObjectStorageBucketPolicy` (`osbp`) | Cluster | Задаёт, из каких namespace разрешён запрос доступа к бакету (deny-by-default). |
| `ObjectStorageBucketAccess` (`osba`) | Namespaced | Запрашивает учётные данные к бакету; пишет стандартный `Secret` с S3-учётками рядом с приложением. |

Бакеты cluster-scoped; учётные данные выдаются по namespace через `ObjectStorageBucketAccess` с проверкой `ObjectStorageBucketPolicy` (доступ без совпадающей политики остаётся в ожидании). Каждый access получает свой ключ, который можно независимо ротировать аннотацией `storage.deckhouse.io/rotate`.

## Профили кластера

Поле `spec.type` выбирает профиль; бэкенд подбирается и сопровождается за вас.

| `spec.type` | Бэкенд | Размещение / данные | Сценарий |
|-------------|--------|---------------------|----------|
| `System` | Garage | DaemonSet на control-plane узлах, `hostPath` | Системные нужды платформы (бэкапы, registry, логи). |
| `Lightweight` | Garage | StatefulSet, PVC на StorageClass | Небольшие прикладные нагрузки. |
| `Full` | SeaweedFS | StatefulSet, PVC | Масштабируемое полнофункциональное хранилище. |
| `Heavy` | Ceph RGW | RADOS Gateway поверх существующего кластера sds-elastic | Переиспользует ёмкость и отказоустойчивость Ceph. |

{{< alert level="info" >}}
Статус реализации: доступны все четыре профиля. `Full` запускает распределённый SeaweedFS (master/volume/filer + S3-gateway; число реплик и репликация масштабируются от `redundancy`). Метаданные filer хранятся в общем PostgreSQL, разворачиваемом модулем [managed-postgres](../../managed-postgres/stable/), благодаря чему filer/S3-gateway работает в нескольких репликах (HA). `Heavy` разворачивает Ceph RGW поверх существующего кластера sds-elastic. Бакеты работают на всех четырёх профилях.

Поэтому профиль `Full` требует включённого модуля `managed-postgres`.
{{< /alert >}}

## Как это работает

- Контроллер реконсилит каждый `ObjectStorageCluster` в data plane бэкенда (workload'ы, сервисы, конфигурация) и публикует в `status` готовность, S3-эндпойнт и ёмкость.
- Контроллер реконсилит каждый `ObjectStorageBucket` в бакет в указанном кластере (без учётных данных).
- Контроллер реконсилит каждый `ObjectStorageBucketAccess` — как только `ObjectStorageBucketPolicy` разрешает его namespace — в ключ доступа с правами на бакет, затем пишет `Secret` (owned by access) со стандартными переменными подключения: `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`.

Пошаговый пример — в разделе [Использование](usage.html).

## Требования

- Профиль `Heavy` требует модуль [`sds-elastic`](/modules/sds-elastic/) с готовым `ElasticCluster`; он используется только для `Heavy`-кластеров.
- Профили `System`, `Lightweight` и `Full` самодостаточны (образы бэкендов поставляются вместе с модулем).
