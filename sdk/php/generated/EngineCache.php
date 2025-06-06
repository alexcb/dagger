<?php

/**
 * This class has been generated by dagger-php-sdk. DO NOT EDIT.
 */

declare(strict_types=1);

namespace Dagger;

/**
 * A cache storage for the Dagger engine
 */
class EngineCache extends Client\AbstractObject implements Client\IdAble
{
    /**
     * The current set of entries in the cache
     */
    public function entrySet(?string $key = ''): EngineCacheEntrySet
    {
        $innerQueryBuilder = new \Dagger\Client\QueryBuilder('entrySet');
        if (null !== $key) {
        $innerQueryBuilder->setArgument('key', $key);
        }
        return new \Dagger\EngineCacheEntrySet($this->client, $this->queryBuilderChain->chain($innerQueryBuilder));
    }

    /**
     * A unique identifier for this EngineCache.
     */
    public function id(): EngineCacheId
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('id');
        return new \Dagger\EngineCacheId((string)$this->queryLeaf($leafQueryBuilder, 'id'));
    }

    /**
     * The maximum bytes to keep in the cache without pruning, after which automatic pruning may kick in.
     */
    public function keepBytes(): int
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('keepBytes');
        return (int)$this->queryLeaf($leafQueryBuilder, 'keepBytes');
    }

    /**
     * The maximum bytes to keep in the cache without pruning.
     */
    public function maxUsedSpace(): int
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('maxUsedSpace');
        return (int)$this->queryLeaf($leafQueryBuilder, 'maxUsedSpace');
    }

    /**
     * The target amount of free disk space the garbage collector will attempt to leave.
     */
    public function minFreeSpace(): int
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('minFreeSpace');
        return (int)$this->queryLeaf($leafQueryBuilder, 'minFreeSpace');
    }

    /**
     * Prune the cache of releaseable entries
     */
    public function prune(?bool $useDefaultPolicy = false): void
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('prune');
        if (null !== $useDefaultPolicy) {
        $leafQueryBuilder->setArgument('useDefaultPolicy', $useDefaultPolicy);
        }
        $this->queryLeaf($leafQueryBuilder, 'prune');
    }

    /**
     * The minimum amount of disk space this policy is guaranteed to retain.
     */
    public function reservedSpace(): int
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('reservedSpace');
        return (int)$this->queryLeaf($leafQueryBuilder, 'reservedSpace');
    }

    /**
     * The target number of bytes to keep when pruning.
     */
    public function targetSpace(): int
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('targetSpace');
        return (int)$this->queryLeaf($leafQueryBuilder, 'targetSpace');
    }
}
