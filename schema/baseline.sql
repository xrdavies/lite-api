--
-- PostgreSQL database dump
--


-- Dumped from database version 17.11
-- Dumped by pg_dump version 17.11

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;


--
-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';


--
-- Name: enforce_openai_long_context_billing_extra(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enforce_openai_long_context_billing_extra() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    parent_effective_value JSONB;
BEGIN
    IF NEW.platform IS DISTINCT FROM 'openai' THEN
        RETURN NEW;
    END IF;

    NEW.extra := COALESCE(NEW.extra, '{}'::jsonb);
    IF NEW.parent_account_id IS NOT NULL AND NEW.quota_dimension = 'spark' THEN
        SELECT CASE
            WHEN parent.platform IS DISTINCT FROM 'openai' THEN 'false'::jsonb
            WHEN NOT (COALESCE(parent.extra, '{}'::jsonb) ? 'openai_long_context_billing_enabled') THEN 'false'::jsonb
            WHEN jsonb_typeof(parent.extra->'openai_long_context_billing_enabled') = 'boolean'
                THEN parent.extra->'openai_long_context_billing_enabled'
            ELSE 'false'::jsonb
        END
        INTO parent_effective_value
        FROM accounts AS parent
        WHERE parent.id = NEW.parent_account_id;

        NEW.extra := jsonb_set(
            NEW.extra,
            '{openai_long_context_billing_enabled}',
            COALESCE(parent_effective_value, 'false'::jsonb),
            true
        );
    ELSIF NOT (NEW.extra ? 'openai_long_context_billing_enabled')
        AND TG_OP = 'UPDATE'
        AND OLD.platform = 'openai'
        AND jsonb_typeof(OLD.extra->'openai_long_context_billing_enabled') = 'boolean' THEN
        NEW.extra := jsonb_set(
            NEW.extra,
            '{openai_long_context_billing_enabled}',
            OLD.extra->'openai_long_context_billing_enabled',
            true
        );
    ELSIF NOT (NEW.extra ? 'openai_long_context_billing_enabled') THEN
        NEW.extra := jsonb_set(
            NEW.extra,
            '{openai_long_context_billing_enabled}',
            'false'::jsonb,
            true
        );
    END IF;

    IF jsonb_typeof(NEW.extra->'openai_long_context_billing_enabled') IS DISTINCT FROM 'boolean' THEN
        RAISE EXCEPTION 'openai_long_context_billing_enabled must be a boolean'
            USING ERRCODE = '22023';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enqueue_allowed_group_auth_cache_invalidation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enqueue_allowed_group_auth_cache_invalidation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_user_id BIGINT;
    target_group_id BIGINT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND (OLD.user_id IS DISTINCT FROM NEW.user_id
            OR OLD.group_id IS DISTINCT FROM NEW.group_id) THEN
        IF EXISTS (
            SELECT 1 FROM groups g
            WHERE g.id = OLD.group_id AND g.is_exclusive = TRUE
        ) THEN
            INSERT INTO auth_cache_invalidation_outbox (cache_key)
            SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
            FROM api_keys AS k
            WHERE k.user_id = OLD.user_id
              AND k.group_id = OLD.group_id
              AND k.deleted_at IS NULL
              AND k.key <> '';
        END IF;
        target_user_id := NEW.user_id;
        target_group_id := NEW.group_id;
    ELSIF TG_OP = 'UPDATE' THEN
        RETURN NEW;
    ELSIF TG_OP = 'INSERT' THEN
        target_user_id := NEW.user_id;
        target_group_id := NEW.group_id;
    ELSE
        target_user_id := OLD.user_id;
        target_group_id := OLD.group_id;
    END IF;

    IF EXISTS (
        SELECT 1 FROM groups g
        WHERE g.id = target_group_id AND g.is_exclusive = TRUE
    ) THEN
        INSERT INTO auth_cache_invalidation_outbox (cache_key)
        SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
        FROM api_keys AS k
        WHERE k.user_id = target_user_id
          AND k.group_id = target_group_id
          AND k.deleted_at IS NULL
          AND k.key <> '';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enqueue_api_key_auth_cache_invalidation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enqueue_api_key_auth_cache_invalidation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        RETURN OLD;
    END IF;

    IF OLD.key IS DISTINCT FROM NEW.key
       OR OLD.status IS DISTINCT FROM NEW.status
       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
       OR OLD.user_id IS DISTINCT FROM NEW.user_id
       OR OLD.group_id IS DISTINCT FROM NEW.group_id
       OR OLD.ip_whitelist IS DISTINCT FROM NEW.ip_whitelist
       OR OLD.ip_blacklist IS DISTINCT FROM NEW.ip_blacklist
       OR OLD.expires_at IS DISTINCT FROM NEW.expires_at THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        IF NEW.deleted_at IS NULL AND NEW.key IS DISTINCT FROM OLD.key THEN
            PERFORM enqueue_auth_cache_invalidation(NEW.key);
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enqueue_auth_cache_invalidation(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enqueue_auth_cache_invalidation(raw_key text) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF raw_key IS NULL OR raw_key = '' THEN
        RETURN;
    END IF;
    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    VALUES (encode(sha256(convert_to(raw_key, 'UTF8')), 'hex'));
END;
$$;


--
-- Name: enqueue_group_auth_cache_invalidation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enqueue_group_auth_cache_invalidation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_group_id BIGINT;
BEGIN
    target_group_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.is_exclusive IS NOT DISTINCT FROM NEW.is_exclusive
       AND OLD.allow_image_generation IS NOT DISTINCT FROM NEW.allow_image_generation
       AND OLD.platform IS NOT DISTINCT FROM NEW.platform
       AND OLD.subscription_type IS NOT DISTINCT FROM NEW.subscription_type
       AND OLD.rate_multiplier IS NOT DISTINCT FROM NEW.rate_multiplier
       AND OLD.peak_rate_enabled IS NOT DISTINCT FROM NEW.peak_rate_enabled
       AND OLD.peak_start IS NOT DISTINCT FROM NEW.peak_start
       AND OLD.peak_end IS NOT DISTINCT FROM NEW.peak_end
       AND OLD.peak_rate_multiplier IS NOT DISTINCT FROM NEW.peak_rate_multiplier
       AND OLD.profit_control_enabled IS NOT DISTINCT FROM NEW.profit_control_enabled
       AND OLD.profit_min_margin IS NOT DISTINCT FROM NEW.profit_min_margin
       AND OLD.profit_safety_buffer IS NOT DISTINCT FROM NEW.profit_safety_buffer
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
    FROM api_keys AS k
    WHERE k.group_id = target_group_id
      AND k.deleted_at IS NULL
      AND k.key <> '';
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enqueue_user_auth_cache_invalidation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enqueue_user_auth_cache_invalidation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_user_id BIGINT;
BEGIN
    target_user_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.role IS NOT DISTINCT FROM NEW.role
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
    FROM api_keys AS k
    WHERE k.user_id = target_user_id
      AND k.deleted_at IS NULL
      AND k.key <> '';
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: invalidate_group_usage_rollup_state(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.invalidate_group_usage_rollup_state() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    affected_date DATE;
    published_before DATE;
    configured_timezone TEXT := current_setting('TimeZone');
BEGIN
    IF TG_OP = 'DELETE' THEN
        affected_date := (OLD.created_at AT TIME ZONE configured_timezone)::date;
    ELSE
        IF OLD.group_id IS NULL THEN
            affected_date := (NEW.created_at AT TIME ZONE configured_timezone)::date;
        ELSIF NEW.group_id IS NULL THEN
            affected_date := (OLD.created_at AT TIME ZONE configured_timezone)::date;
        ELSE
            affected_date := LEAST(
                (OLD.created_at AT TIME ZONE configured_timezone)::date,
                (NEW.created_at AT TIME ZONE configured_timezone)::date
            );
        END IF;
    END IF;

    SELECT closed_before
    INTO published_before
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR UPDATE;

    IF published_before > affected_date THEN
        UPDATE usage_group_rollup_state
        SET closed_before = LEAST(closed_before, affected_date),
            updated_at = NOW()
        WHERE id = 1;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: invalidate_group_usage_rollup_state_after_insert(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.invalidate_group_usage_rollup_state_after_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    affected_date DATE;
    published_before DATE;
    configured_timezone TEXT := current_setting('TimeZone');
BEGIN
    SELECT MIN((created_at AT TIME ZONE configured_timezone)::date)
    INTO affected_date
    FROM inserted_usage_logs
    WHERE group_id IS NOT NULL;

    IF affected_date IS NULL THEN
        RETURN NULL;
    END IF;

    SELECT closed_before
    INTO published_before
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR KEY SHARE;

    IF published_before > affected_date THEN
        UPDATE usage_group_rollup_state
        SET closed_before = LEAST(closed_before, affected_date),
            updated_at = NOW()
        WHERE id = 1;
    END IF;

    RETURN NULL;
END;
$$;


--
-- Name: propagate_openai_long_context_billing_extra_to_shadows(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.propagate_openai_long_context_billing_extra_to_shadows() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    WITH updated_shadows AS (
        UPDATE accounts AS shadow
        SET extra = jsonb_set(
            COALESCE(shadow.extra, '{}'::jsonb),
            '{openai_long_context_billing_enabled}',
            NEW.extra->'openai_long_context_billing_enabled',
            true
        )
        WHERE shadow.parent_account_id = NEW.id
          AND shadow.platform = 'openai'
          AND shadow.quota_dimension = 'spark'
          AND shadow.extra->'openai_long_context_billing_enabled'
              IS DISTINCT FROM NEW.extra->'openai_long_context_billing_enabled'
        RETURNING shadow.id
    )
    INSERT INTO scheduler_outbox (event_type, account_id)
    SELECT 'account_changed', id
    FROM updated_shadows;
    RETURN NULL;
END;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: account_groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.account_groups (
    account_id bigint NOT NULL,
    group_id bigint NOT NULL,
    priority integer DEFAULT 50 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.accounts (
    id bigint NOT NULL,
    name character varying(100) NOT NULL,
    platform character varying(50) NOT NULL,
    type character varying(20) NOT NULL,
    credentials jsonb DEFAULT '{}'::jsonb NOT NULL,
    extra jsonb DEFAULT '{}'::jsonb NOT NULL,
    proxy_id bigint,
    concurrency integer DEFAULT 3 NOT NULL,
    priority integer DEFAULT 50 NOT NULL,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    error_message text,
    last_used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    schedulable boolean DEFAULT true NOT NULL,
    rate_limited_at timestamp with time zone,
    rate_limit_reset_at timestamp with time zone,
    overload_until timestamp with time zone,
    session_window_start timestamp with time zone,
    session_window_end timestamp with time zone,
    session_window_status character varying(20),
    temp_unschedulable_until timestamp with time zone,
    temp_unschedulable_reason text,
    notes text,
    expires_at timestamp with time zone,
    auto_pause_on_expired boolean DEFAULT true NOT NULL,
    rate_multiplier numeric(10,4) DEFAULT 1.0 NOT NULL,
    load_factor integer,
    proxy_fallback_origin_id bigint,
    parent_account_id bigint,
    quota_dimension character varying(20) DEFAULT 'global'::character varying NOT NULL,
    CONSTRAINT chk_accounts_parent_dimension CHECK ((((parent_account_id IS NULL) AND ((quota_dimension)::text = 'global'::text)) OR ((parent_account_id IS NOT NULL) AND ((quota_dimension)::text <> 'global'::text)))),
    CONSTRAINT chk_accounts_parent_not_self CHECK (((parent_account_id IS NULL) OR (parent_account_id <> id))),
    CONSTRAINT chk_accounts_quota_dimension CHECK (((quota_dimension)::text = ANY ((ARRAY['global'::character varying, 'spark'::character varying])::text[])))
);


--
-- Name: COLUMN accounts.temp_unschedulable_until; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.accounts.temp_unschedulable_until IS '临时不可调度状态解除时间，当触发临时不可调度规则时设置（基于错误码或错误描述关键词）';


--
-- Name: COLUMN accounts.temp_unschedulable_reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.accounts.temp_unschedulable_reason IS '临时不可调度原因，记录触发临时不可调度的具体原因（用于排障和审计）';


--
-- Name: COLUMN accounts.notes; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.accounts.notes IS 'Admin-only notes for account';


--
-- Name: COLUMN accounts.expires_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.accounts.expires_at IS 'Account expiration time (NULL means no expiration).';


--
-- Name: COLUMN accounts.auto_pause_on_expired; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.accounts.auto_pause_on_expired IS 'Auto pause scheduling when account expires.';


--
-- Name: accounts_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.accounts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: accounts_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.accounts_id_seq OWNED BY public.accounts.id;


--
-- Name: announcement_reads; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.announcement_reads (
    id bigint NOT NULL,
    announcement_id bigint NOT NULL,
    user_id bigint NOT NULL,
    read_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE announcement_reads; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.announcement_reads IS '公告已读记录';


--
-- Name: COLUMN announcement_reads.read_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.announcement_reads.read_at IS '用户首次已读时间';


--
-- Name: announcement_reads_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.announcement_reads_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: announcement_reads_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.announcement_reads_id_seq OWNED BY public.announcement_reads.id;


--
-- Name: announcements; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.announcements (
    id bigint NOT NULL,
    title character varying(200) NOT NULL,
    content text NOT NULL,
    status character varying(20) DEFAULT 'draft'::character varying NOT NULL,
    targeting jsonb DEFAULT '{}'::jsonb NOT NULL,
    starts_at timestamp with time zone,
    ends_at timestamp with time zone,
    created_by bigint,
    updated_by bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    notify_mode character varying(20) DEFAULT 'silent'::character varying NOT NULL
);


--
-- Name: TABLE announcements; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.announcements IS '系统公告';


--
-- Name: COLUMN announcements.status; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.announcements.status IS '状态: draft, active, archived';


--
-- Name: COLUMN announcements.targeting; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.announcements.targeting IS '展示条件（JSON 规则）';


--
-- Name: COLUMN announcements.starts_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.announcements.starts_at IS '开始展示时间（为空表示立即生效）';


--
-- Name: COLUMN announcements.ends_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.announcements.ends_at IS '结束展示时间（为空表示永久生效）';


--
-- Name: announcements_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.announcements_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: announcements_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.announcements_id_seq OWNED BY public.announcements.id;


--
-- Name: api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_keys (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    key character varying(128) NOT NULL,
    name character varying(100) NOT NULL,
    group_id bigint,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    ip_whitelist jsonb,
    ip_blacklist jsonb,
    quota numeric(20,8) DEFAULT 0 NOT NULL,
    quota_used numeric(20,8) DEFAULT 0 NOT NULL,
    expires_at timestamp with time zone,
    last_used_at timestamp with time zone,
    rate_limit_5h numeric(20,8) DEFAULT 0 NOT NULL,
    rate_limit_1d numeric(20,8) DEFAULT 0 NOT NULL,
    rate_limit_7d numeric(20,8) DEFAULT 0 NOT NULL,
    usage_5h numeric(20,8) DEFAULT 0 NOT NULL,
    usage_1d numeric(20,8) DEFAULT 0 NOT NULL,
    usage_7d numeric(20,8) DEFAULT 0 NOT NULL,
    window_5h_start timestamp with time zone,
    window_1d_start timestamp with time zone,
    window_7d_start timestamp with time zone
);


--
-- Name: COLUMN api_keys.ip_whitelist; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.api_keys.ip_whitelist IS 'JSON array of allowed IPs/CIDRs, e.g. ["192.168.1.100", "10.0.0.0/8"]';


--
-- Name: COLUMN api_keys.ip_blacklist; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.api_keys.ip_blacklist IS 'JSON array of blocked IPs/CIDRs, e.g. ["1.2.3.4", "5.6.0.0/16"]';


--
-- Name: COLUMN api_keys.quota; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.api_keys.quota IS 'Quota limit in USD for this API key (0 = unlimited)';


--
-- Name: COLUMN api_keys.quota_used; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.api_keys.quota_used IS 'Used quota amount in USD';


--
-- Name: COLUMN api_keys.expires_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.api_keys.expires_at IS 'Expiration time for this API key (null = never expires)';


--
-- Name: api_keys_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.api_keys_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: api_keys_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.api_keys_id_seq OWNED BY public.api_keys.id;


--
-- Name: audit_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_logs (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    actor_user_id bigint,
    actor_email character varying(255) DEFAULT ''::character varying NOT NULL,
    actor_role character varying(32) DEFAULT ''::character varying NOT NULL,
    auth_method character varying(32) DEFAULT ''::character varying NOT NULL,
    credential_masked character varying(160) DEFAULT ''::character varying NOT NULL,
    action character varying(128) DEFAULT ''::character varying NOT NULL,
    method character varying(16) DEFAULT ''::character varying NOT NULL,
    path character varying(512) DEFAULT ''::character varying NOT NULL,
    request_id character varying(64) DEFAULT ''::character varying NOT NULL,
    client_ip character varying(64) DEFAULT ''::character varying NOT NULL,
    user_agent character varying(512) DEFAULT ''::character varying NOT NULL,
    request_body text DEFAULT ''::text NOT NULL,
    status_code integer DEFAULT 0 NOT NULL,
    latency_ms bigint DEFAULT 0 NOT NULL,
    extra jsonb DEFAULT '{}'::jsonb NOT NULL
);


--
-- Name: audit_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.audit_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: audit_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.audit_logs_id_seq OWNED BY public.audit_logs.id;


--
-- Name: auth_cache_invalidation_outbox; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auth_cache_invalidation_outbox (
    id bigint NOT NULL,
    cache_key character(64) NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    available_at timestamp with time zone DEFAULT now() NOT NULL,
    delivery_stage smallint DEFAULT 0 NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    last_error text,
    claimed_at timestamp with time zone,
    claimed_by text,
    CONSTRAINT auth_cache_invalidation_outbox_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT auth_cache_invalidation_outbox_cache_key_check CHECK ((cache_key ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT auth_cache_invalidation_outbox_delivery_stage_check CHECK ((delivery_stage = ANY (ARRAY[0, 1])))
);


--
-- Name: TABLE auth_cache_invalidation_outbox; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.auth_cache_invalidation_outbox IS 'Durable cross-instance auth cache invalidations; cache_key is SHA-256 hex, never plaintext API key';


--
-- Name: auth_cache_invalidation_outbox_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.auth_cache_invalidation_outbox_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: auth_cache_invalidation_outbox_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.auth_cache_invalidation_outbox_id_seq OWNED BY public.auth_cache_invalidation_outbox.id;


--
-- Name: auth_identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auth_identities (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    provider_type character varying(20) NOT NULL,
    provider_key text NOT NULL,
    provider_subject text NOT NULL,
    verified_at timestamp with time zone,
    issuer text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT auth_identities_metadata_is_object_check CHECK ((jsonb_typeof(metadata) = 'object'::text)),
    CONSTRAINT auth_identities_provider_type_check CHECK (((provider_type)::text = ANY ((ARRAY['email'::character varying, 'linuxdo'::character varying, 'wechat'::character varying, 'oidc'::character varying, 'github'::character varying, 'google'::character varying, 'dingtalk'::character varying])::text[])))
);


--
-- Name: auth_identities_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.auth_identities_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: auth_identities_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.auth_identities_id_seq OWNED BY public.auth_identities.id;


--
-- Name: auth_identity_channels; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auth_identity_channels (
    id bigint NOT NULL,
    identity_id bigint NOT NULL,
    provider_type character varying(20) NOT NULL,
    provider_key text NOT NULL,
    channel character varying(20) NOT NULL,
    channel_app_id text NOT NULL,
    channel_subject text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT auth_identity_channels_metadata_is_object_check CHECK ((jsonb_typeof(metadata) = 'object'::text)),
    CONSTRAINT auth_identity_channels_provider_type_check CHECK (((provider_type)::text = ANY ((ARRAY['email'::character varying, 'linuxdo'::character varying, 'wechat'::character varying, 'oidc'::character varying, 'github'::character varying, 'google'::character varying, 'dingtalk'::character varying])::text[])))
);


--
-- Name: auth_identity_channels_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.auth_identity_channels_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: auth_identity_channels_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.auth_identity_channels_id_seq OWNED BY public.auth_identity_channels.id;


--
-- Name: auth_identity_migration_reports; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auth_identity_migration_reports (
    id bigint NOT NULL,
    report_type character varying(80) NOT NULL,
    report_key text NOT NULL,
    details jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    resolved_by_user_id bigint,
    resolution_note text DEFAULT ''::text NOT NULL,
    CONSTRAINT auth_identity_migration_reports_details_is_object_check CHECK ((jsonb_typeof(details) = 'object'::text))
);


--
-- Name: auth_identity_migration_reports_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.auth_identity_migration_reports_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: auth_identity_migration_reports_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.auth_identity_migration_reports_id_seq OWNED BY public.auth_identity_migration_reports.id;


--
-- Name: batch_image_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.batch_image_events (
    id bigint NOT NULL,
    job_id character varying(64) NOT NULL,
    event_type character varying(64) NOT NULL,
    payload jsonb,
    event_hash character varying(128),
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: batch_image_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.batch_image_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: batch_image_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.batch_image_events_id_seq OWNED BY public.batch_image_events.id;


--
-- Name: batch_image_items; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.batch_image_items (
    id bigint NOT NULL,
    job_id character varying(64) NOT NULL,
    custom_id character varying(255) NOT NULL,
    status character varying(32) NOT NULL,
    request_hash character varying(128),
    prompt_preview text,
    provider_source_object character varying(1024),
    source_line_number integer,
    source_byte_offset bigint,
    source_byte_length bigint,
    mime_type character varying(128),
    file_extension character varying(32),
    image_count integer DEFAULT 0 NOT NULL,
    error_code character varying(128),
    error_message text,
    billed_amount numeric(20,10),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    indexed_at timestamp with time zone
);


--
-- Name: batch_image_items_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.batch_image_items_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: batch_image_items_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.batch_image_items_id_seq OWNED BY public.batch_image_items.id;


--
-- Name: batch_image_jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.batch_image_jobs (
    id bigint NOT NULL,
    batch_id character varying(64) NOT NULL,
    user_id bigint NOT NULL,
    api_key_id bigint,
    account_id bigint,
    provider character varying(32) NOT NULL,
    model character varying(128) NOT NULL,
    status character varying(32) DEFAULT 'created'::character varying NOT NULL,
    provider_job_name character varying(512),
    gcs_input_uri character varying(1024),
    gcs_output_uri character varying(1024),
    item_count integer NOT NULL,
    success_count integer DEFAULT 0 NOT NULL,
    fail_count integer DEFAULT 0 NOT NULL,
    cancelled_count integer DEFAULT 0 NOT NULL,
    estimated_cost numeric(20,10) DEFAULT 0 NOT NULL,
    hold_amount numeric(20,10),
    actual_cost numeric(20,10),
    currency character varying(16) DEFAULT 'USD'::character varying NOT NULL,
    hold_id character varying(128),
    idempotency_key character varying(255),
    request_hash character varying(128),
    manifest_hash character varying(128),
    retry_count integer DEFAULT 0 NOT NULL,
    version integer DEFAULT 0 NOT NULL,
    output_expires_at timestamp with time zone,
    input_deleted_at timestamp with time zone,
    output_deleted_at timestamp with time zone,
    last_error_code character varying(128),
    last_error_message text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    submitted_at timestamp with time zone,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    settled_at timestamp with time zone,
    provider_input_ref character varying(1024),
    provider_output_ref character varying(1024),
    base_unit_price numeric(20,10) DEFAULT 0 NOT NULL,
    group_rate_multiplier numeric(10,4) DEFAULT 1.0 NOT NULL,
    account_rate_multiplier numeric(10,4) DEFAULT 1.0 NOT NULL,
    batch_discount_multiplier numeric(10,4) DEFAULT 0.5 NOT NULL,
    hold_multiplier numeric(10,4) DEFAULT 0.6 NOT NULL,
    billable_unit_price numeric(20,10) DEFAULT 0 NOT NULL,
    hold_unit_price numeric(20,10) DEFAULT 0 NOT NULL,
    pricing_snapshot_version integer DEFAULT 0 NOT NULL,
    downloaded_at timestamp with time zone,
    user_deleted_at timestamp with time zone,
    task_name character varying(255) DEFAULT ''::character varying NOT NULL,
    parent_batch_id character varying(64),
    session_id character varying(255)
);


--
-- Name: COLUMN batch_image_jobs.base_unit_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.base_unit_price IS '提交时快照的基础批量图片单价';


--
-- Name: COLUMN batch_image_jobs.group_rate_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.group_rate_multiplier IS '提交时快照的分组/用户专属图片倍率';


--
-- Name: COLUMN batch_image_jobs.account_rate_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.account_rate_multiplier IS '提交时快照的账号倍率';


--
-- Name: COLUMN batch_image_jobs.batch_discount_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.batch_discount_multiplier IS '提交时快照的批量折扣倍率';


--
-- Name: COLUMN batch_image_jobs.hold_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.hold_multiplier IS '提交时快照的冻结价格比例，按普通生图原价乘以该比例冻结';


--
-- Name: COLUMN batch_image_jobs.billable_unit_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.billable_unit_price IS '提交时快照的实际结算单价';


--
-- Name: COLUMN batch_image_jobs.hold_unit_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.hold_unit_price IS '提交时快照的冻结单价';


--
-- Name: COLUMN batch_image_jobs.pricing_snapshot_version; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.pricing_snapshot_version IS '批量图片任务价格快照版本；0 表示旧任务无快照';


--
-- Name: COLUMN batch_image_jobs.downloaded_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.downloaded_at IS '用户首次成功下载批量图片 ZIP 的时间';


--
-- Name: COLUMN batch_image_jobs.user_deleted_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.user_deleted_at IS '用户侧删除/隐藏任务记录的时间；账务记录仍保留';


--
-- Name: COLUMN batch_image_jobs.task_name; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.task_name IS '用户填写的批量生图任务名称；提交时为空则默认写入当前时间';


--
-- Name: COLUMN batch_image_jobs.parent_batch_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.batch_image_jobs.parent_batch_id IS '父批量生图任务 ID；失败项重试等子任务挂在主任务下展示';


--
-- Name: batch_image_jobs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.batch_image_jobs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: batch_image_jobs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.batch_image_jobs_id_seq OWNED BY public.batch_image_jobs.id;


--
-- Name: billing_usage_entries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.billing_usage_entries (
    id bigint NOT NULL,
    usage_log_id bigint NOT NULL,
    user_id bigint NOT NULL,
    api_key_id bigint NOT NULL,
    subscription_id bigint,
    billing_type smallint NOT NULL,
    applied boolean DEFAULT true NOT NULL,
    delta_usd numeric(20,10) DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: billing_usage_entries_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.billing_usage_entries_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: billing_usage_entries_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.billing_usage_entries_id_seq OWNED BY public.billing_usage_entries.id;


--
-- Name: channel_account_stats_model_pricing; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_account_stats_model_pricing (
    id bigint NOT NULL,
    rule_id bigint NOT NULL,
    platform character varying(50) DEFAULT ''::character varying NOT NULL,
    models jsonb DEFAULT '[]'::jsonb NOT NULL,
    billing_mode character varying(20) DEFAULT 'token'::character varying NOT NULL,
    input_price numeric(20,10),
    output_price numeric(20,10),
    cache_write_price numeric(20,10),
    cache_read_price numeric(20,10),
    image_output_price numeric(20,10),
    per_request_price numeric(20,10),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    cache_write_1h_price numeric(20,12),
    reasoning_effort_multipliers jsonb DEFAULT '{}'::jsonb NOT NULL
);


--
-- Name: COLUMN channel_account_stats_model_pricing.cache_write_1h_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_account_stats_model_pricing.cache_write_1h_price IS '1h cache write price per token for account stats pricing';


--
-- Name: COLUMN channel_account_stats_model_pricing.reasoning_effort_multipliers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_account_stats_model_pricing.reasoning_effort_multipliers IS 'Custom account statistics billing multipliers by reasoning effort; omitted efforts use 1x';


--
-- Name: channel_account_stats_model_pricing_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_account_stats_model_pricing_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_account_stats_model_pricing_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_account_stats_model_pricing_id_seq OWNED BY public.channel_account_stats_model_pricing.id;


--
-- Name: channel_account_stats_pricing_intervals; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_account_stats_pricing_intervals (
    id bigint NOT NULL,
    pricing_id bigint NOT NULL,
    min_tokens integer DEFAULT 0 NOT NULL,
    max_tokens integer,
    tier_label character varying(50),
    input_price numeric(20,12),
    output_price numeric(20,12),
    cache_write_price numeric(20,12),
    cache_read_price numeric(20,12),
    per_request_price numeric(20,12),
    sort_order integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    cache_write_1h_price numeric(20,12)
);


--
-- Name: COLUMN channel_account_stats_pricing_intervals.cache_write_1h_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_account_stats_pricing_intervals.cache_write_1h_price IS 'Interval-specific 1h cache write price per token for account stats pricing';


--
-- Name: channel_account_stats_pricing_intervals_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_account_stats_pricing_intervals_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_account_stats_pricing_intervals_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_account_stats_pricing_intervals_id_seq OWNED BY public.channel_account_stats_pricing_intervals.id;


--
-- Name: channel_account_stats_pricing_rules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_account_stats_pricing_rules (
    id bigint NOT NULL,
    channel_id bigint NOT NULL,
    name character varying(100) DEFAULT ''::character varying NOT NULL,
    group_ids bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    account_ids bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: channel_account_stats_pricing_rules_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_account_stats_pricing_rules_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_account_stats_pricing_rules_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_account_stats_pricing_rules_id_seq OWNED BY public.channel_account_stats_pricing_rules.id;


--
-- Name: channel_groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_groups (
    id bigint NOT NULL,
    channel_id bigint NOT NULL,
    group_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE channel_groups; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.channel_groups IS '渠道-分组关联表：每个分组最多属于一个渠道';


--
-- Name: channel_groups_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_groups_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_groups_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_groups_id_seq OWNED BY public.channel_groups.id;


--
-- Name: channel_model_pricing; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_model_pricing (
    id bigint NOT NULL,
    channel_id bigint NOT NULL,
    models jsonb DEFAULT '[]'::jsonb NOT NULL,
    input_price numeric(20,12),
    output_price numeric(20,12),
    cache_write_price numeric(20,12),
    cache_read_price numeric(20,12),
    image_output_price numeric(20,8),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    billing_mode character varying(20) DEFAULT 'token'::character varying NOT NULL,
    per_request_price numeric(20,10),
    platform character varying(50) DEFAULT 'anthropic'::character varying NOT NULL,
    image_input_price numeric(20,12),
    time_pricing jsonb,
    fast_multiplier numeric(12,6),
    flex_multiplier numeric(12,6),
    cache_write_1h_price numeric(20,12),
    max_reasoning_effort_multiplier numeric(10,4),
    reasoning_effort_multipliers jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT channel_model_pricing_fast_multiplier_positive CHECK (((fast_multiplier IS NULL) OR (fast_multiplier > (0)::numeric))),
    CONSTRAINT channel_model_pricing_flex_multiplier_positive CHECK (((flex_multiplier IS NULL) OR (flex_multiplier > (0)::numeric))),
    CONSTRAINT chk_channel_model_pricing_max_reasoning_effort_multiplier_posit CHECK (((max_reasoning_effort_multiplier IS NULL) OR (max_reasoning_effort_multiplier > (0)::numeric)))
);


--
-- Name: TABLE channel_model_pricing; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.channel_model_pricing IS '渠道模型定价：一条定价可绑定多个模型，价格一致';


--
-- Name: COLUMN channel_model_pricing.models; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.models IS '绑定的模型列表，JSON 数组，如 ["claude-opus-4-6","claude-opus-4-6-thinking"]';


--
-- Name: COLUMN channel_model_pricing.input_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.input_price IS '每 token 输入价格（USD），NULL 表示使用默认';


--
-- Name: COLUMN channel_model_pricing.output_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.output_price IS '每 token 输出价格（USD），NULL 表示使用默认';


--
-- Name: COLUMN channel_model_pricing.cache_write_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.cache_write_price IS '缓存写入每 token 价格，NULL 表示使用默认';


--
-- Name: COLUMN channel_model_pricing.cache_read_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.cache_read_price IS '缓存读取每 token 价格，NULL 表示使用默认';


--
-- Name: COLUMN channel_model_pricing.image_output_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.image_output_price IS '图片输出价格（Gemini Image 等），NULL 表示使用默认';


--
-- Name: COLUMN channel_model_pricing.billing_mode; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.billing_mode IS '计费模式：token（按 token 区间计费）、per_request（按次计费）、image（图片计费）';


--
-- Name: COLUMN channel_model_pricing.time_pricing; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.time_pricing IS 'Optional IANA timezone and recurring daily multiplier periods for channel token pricing';


--
-- Name: COLUMN channel_model_pricing.fast_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.fast_multiplier IS 'Fast/priority service tier multiplier applied to the selected standard channel price';


--
-- Name: COLUMN channel_model_pricing.flex_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.flex_multiplier IS 'Flex service tier multiplier applied to the selected standard channel price';


--
-- Name: COLUMN channel_model_pricing.cache_write_1h_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.cache_write_1h_price IS '1h cache write price per token; NULL preserves legacy cache_write_price behavior';


--
-- Name: COLUMN channel_model_pricing.max_reasoning_effort_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.max_reasoning_effort_multiplier IS 'Billing/quota multiplier applied when the forwarded reasoning effort is max';


--
-- Name: COLUMN channel_model_pricing.reasoning_effort_multipliers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_model_pricing.reasoning_effort_multipliers IS 'Custom billing multipliers by reasoning effort; omitted efforts use 1x';


--
-- Name: channel_model_pricing_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_model_pricing_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_model_pricing_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_model_pricing_id_seq OWNED BY public.channel_model_pricing.id;


--
-- Name: channel_monitor_aggregation_watermark; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_aggregation_watermark (
    id integer DEFAULT 1 NOT NULL,
    last_aggregated_date date,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT channel_monitor_aggregation_watermark_singleton CHECK ((id = 1))
);


--
-- Name: channel_monitor_daily_rollups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_daily_rollups (
    id bigint NOT NULL,
    monitor_id bigint NOT NULL,
    model character varying(200) NOT NULL,
    bucket_date date NOT NULL,
    total_checks integer DEFAULT 0 NOT NULL,
    ok_count integer DEFAULT 0 NOT NULL,
    operational_count integer DEFAULT 0 NOT NULL,
    degraded_count integer DEFAULT 0 NOT NULL,
    failed_count integer DEFAULT 0 NOT NULL,
    error_count integer DEFAULT 0 NOT NULL,
    sum_latency_ms bigint DEFAULT 0 NOT NULL,
    count_latency integer DEFAULT 0 NOT NULL,
    sum_ping_latency_ms bigint DEFAULT 0 NOT NULL,
    count_ping_latency integer DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: channel_monitor_daily_rollups_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_monitor_daily_rollups_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_monitor_daily_rollups_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_monitor_daily_rollups_id_seq OWNED BY public.channel_monitor_daily_rollups.id;


--
-- Name: channel_monitor_histories; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_histories (
    id bigint NOT NULL,
    monitor_id bigint NOT NULL,
    model character varying(200) NOT NULL,
    status character varying(20) NOT NULL,
    latency_ms integer,
    ping_latency_ms integer,
    message character varying(500) DEFAULT ''::character varying NOT NULL,
    checked_at timestamp with time zone DEFAULT now() NOT NULL,
    quota jsonb,
    CONSTRAINT channel_monitor_histories_status_check CHECK (((status)::text = ANY ((ARRAY['operational'::character varying, 'degraded'::character varying, 'failed'::character varying, 'error'::character varying])::text[])))
);


--
-- Name: COLUMN channel_monitor_histories.quota; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_monitor_histories.quota IS '配额模式监控的归一化配额快照（domain.MonitorQuotaSnapshot）；探活模式为 NULL';


--
-- Name: channel_monitor_histories_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_monitor_histories_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_monitor_histories_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_monitor_histories_id_seq OWNED BY public.channel_monitor_histories.id;


--
-- Name: channel_monitor_request_templates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_request_templates (
    id bigint NOT NULL,
    name character varying(100) NOT NULL,
    provider character varying(20) NOT NULL,
    description character varying(500) DEFAULT ''::character varying NOT NULL,
    extra_headers jsonb DEFAULT '{}'::jsonb NOT NULL,
    body_override_mode character varying(10) DEFAULT 'off'::character varying NOT NULL,
    body_override jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    api_mode character varying(32) DEFAULT 'chat_completions'::character varying NOT NULL,
    CONSTRAINT channel_monitor_request_templates_api_mode_check CHECK (((api_mode)::text = ANY ((ARRAY['chat_completions'::character varying, 'responses'::character varying])::text[]))),
    CONSTRAINT channel_monitor_request_templates_body_mode_check CHECK (((body_override_mode)::text = ANY ((ARRAY['off'::character varying, 'merge'::character varying, 'replace'::character varying])::text[]))),
    CONSTRAINT channel_monitor_request_templates_provider_check CHECK (((provider)::text = ANY ((ARRAY['openai'::character varying, 'anthropic'::character varying, 'gemini'::character varying, 'grok'::character varying, 'antigravity'::character varying, 'kimi'::character varying, 'zhipu'::character varying, 'deepseek'::character varying, 'minimax'::character varying, 'opencode_go'::character varying])::text[])))
);


--
-- Name: channel_monitor_request_templates_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_monitor_request_templates_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_monitor_request_templates_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_monitor_request_templates_id_seq OWNED BY public.channel_monitor_request_templates.id;


--
-- Name: channel_monitor_v2_config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_config (
    id smallint DEFAULT 1 NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    refresh_interval_seconds integer DEFAULT 60 NOT NULL,
    platforms jsonb DEFAULT '[{"models": [], "enabled": true, "platform": "anthropic"}, {"models": [], "enabled": true, "platform": "openai"}, {"models": [], "enabled": true, "platform": "grok"}, {"models": [], "enabled": true, "platform": "kiro"}, {"models": [], "enabled": true, "platform": "gemini"}, {"models": [], "enabled": true, "platform": "antigravity"}]'::jsonb NOT NULL,
    group_ids bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    updated_by bigint,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    ignored_error_categories text[] DEFAULT '{}'::text[] NOT NULL,
    health_thresholds jsonb DEFAULT '{"ttft_weight": 0.20, "cache_weight": 0.20, "error_weight": 0.60, "minimum_sample": 50, "target_ttft_ms": 3000, "warning_ttft_ms": 8000, "critical_ttft_ms": 20000, "warning_cache_rate": 0, "warning_error_rate": 0.05, "critical_cache_rate": 0, "critical_error_rate": 0.20}'::jsonb NOT NULL,
    CONSTRAINT channel_monitor_v2_config_id_check CHECK ((id = 1)),
    CONSTRAINT channel_monitor_v2_config_refresh_interval_seconds_check CHECK ((refresh_interval_seconds = ANY (ARRAY[60, 300])))
);


--
-- Name: channel_monitor_v2_error_metrics_1m; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_error_metrics_1m (
    bucket_start timestamp with time zone NOT NULL,
    platform text NOT NULL,
    group_id bigint DEFAULT 0 NOT NULL,
    model text NOT NULL,
    error_category text NOT NULL,
    taxonomy_version smallint NOT NULL,
    error_requests bigint DEFAULT 0 NOT NULL
);


--
-- Name: COLUMN channel_monitor_v2_error_metrics_1m.taxonomy_version; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_monitor_v2_error_metrics_1m.taxonomy_version IS 'Version of the ordered error classification rules used for this aggregate.';


--
-- Name: channel_monitor_v2_error_metrics_rollup; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_error_metrics_rollup (
    bucket_start timestamp with time zone NOT NULL,
    bucket_seconds integer NOT NULL,
    platform text NOT NULL,
    group_id bigint DEFAULT 0 NOT NULL,
    model text NOT NULL,
    error_category text NOT NULL,
    taxonomy_version smallint NOT NULL,
    error_requests bigint DEFAULT 0 NOT NULL,
    CONSTRAINT channel_monitor_v2_error_metrics_rollup_bucket_seconds_check CHECK ((bucket_seconds = ANY (ARRAY[300, 3600, 43200, 86400])))
);


--
-- Name: channel_monitor_v2_latency_histograms_1m; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_latency_histograms_1m (
    bucket_start timestamp with time zone NOT NULL,
    platform text NOT NULL,
    group_id bigint DEFAULT 0 NOT NULL,
    model text NOT NULL,
    user_id bigint DEFAULT 0 NOT NULL,
    metric text NOT NULL,
    upper_bound_ms integer NOT NULL,
    sample_count bigint DEFAULT 0 NOT NULL,
    CONSTRAINT channel_monitor_v2_latency_histograms_1m_metric_check CHECK ((metric = ANY (ARRAY['ttft'::text, 'duration'::text])))
);


--
-- Name: channel_monitor_v2_latency_histograms_rollup; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_latency_histograms_rollup (
    bucket_start timestamp with time zone NOT NULL,
    bucket_seconds integer NOT NULL,
    platform text NOT NULL,
    group_id bigint DEFAULT 0 NOT NULL,
    model text NOT NULL,
    user_id bigint DEFAULT 0 NOT NULL,
    metric text NOT NULL,
    upper_bound_ms integer NOT NULL,
    sample_count bigint DEFAULT 0 NOT NULL,
    CONSTRAINT channel_monitor_v2_latency_histograms_roll_bucket_seconds_check CHECK ((bucket_seconds = ANY (ARRAY[300, 3600, 43200, 86400]))),
    CONSTRAINT channel_monitor_v2_latency_histograms_rollup_metric_check CHECK ((metric = ANY (ARRAY['ttft'::text, 'duration'::text])))
);


--
-- Name: channel_monitor_v2_metrics_1m; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_metrics_1m (
    bucket_start timestamp with time zone NOT NULL,
    platform text NOT NULL,
    group_id bigint DEFAULT 0 NOT NULL,
    model text NOT NULL,
    success_requests bigint DEFAULT 0 NOT NULL,
    error_requests bigint DEFAULT 0 NOT NULL,
    upstream_affected_requests bigint DEFAULT 0 NOT NULL,
    upstream_attempt_count bigint DEFAULT 0 NOT NULL,
    input_tokens bigint DEFAULT 0 NOT NULL,
    output_tokens bigint DEFAULT 0 NOT NULL,
    cache_creation_tokens bigint DEFAULT 0 NOT NULL,
    cache_read_tokens bigint DEFAULT 0 NOT NULL,
    ttft_sum_ms bigint DEFAULT 0 NOT NULL,
    ttft_count bigint DEFAULT 0 NOT NULL,
    duration_sum_ms bigint DEFAULT 0 NOT NULL,
    duration_count bigint DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE channel_monitor_v2_metrics_1m; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.channel_monitor_v2_metrics_1m IS 'One-minute passive channel health facts derived from real user requests; never from active probes.';


--
-- Name: channel_monitor_v2_metrics_rollup; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_metrics_rollup (
    bucket_start timestamp with time zone NOT NULL,
    bucket_seconds integer NOT NULL,
    platform text NOT NULL,
    group_id bigint DEFAULT 0 NOT NULL,
    model text NOT NULL,
    success_requests bigint DEFAULT 0 NOT NULL,
    error_requests bigint DEFAULT 0 NOT NULL,
    upstream_affected_requests bigint DEFAULT 0 NOT NULL,
    upstream_attempt_count bigint DEFAULT 0 NOT NULL,
    input_tokens bigint DEFAULT 0 NOT NULL,
    output_tokens bigint DEFAULT 0 NOT NULL,
    cache_creation_tokens bigint DEFAULT 0 NOT NULL,
    cache_read_tokens bigint DEFAULT 0 NOT NULL,
    ttft_sum_ms bigint DEFAULT 0 NOT NULL,
    ttft_count bigint DEFAULT 0 NOT NULL,
    duration_sum_ms bigint DEFAULT 0 NOT NULL,
    duration_count bigint DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT channel_monitor_v2_metrics_rollup_bucket_seconds_check CHECK ((bucket_seconds = ANY (ARRAY[300, 3600, 43200, 86400])))
);


--
-- Name: TABLE channel_monitor_v2_metrics_rollup; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.channel_monitor_v2_metrics_rollup IS 'Precomputed fixed UI buckets derived from channel_monitor_v2_metrics_1m for 24h/7d/30d views.';


--
-- Name: channel_monitor_v2_user_metrics_1m; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_user_metrics_1m (
    bucket_start timestamp with time zone NOT NULL,
    platform text NOT NULL,
    group_id bigint DEFAULT 0 NOT NULL,
    model text NOT NULL,
    user_id bigint NOT NULL,
    success_requests bigint DEFAULT 0 NOT NULL,
    error_requests bigint DEFAULT 0 NOT NULL,
    input_tokens bigint DEFAULT 0 NOT NULL,
    output_tokens bigint DEFAULT 0 NOT NULL,
    cache_creation_tokens bigint DEFAULT 0 NOT NULL,
    cache_read_tokens bigint DEFAULT 0 NOT NULL,
    ttft_sum_ms bigint DEFAULT 0 NOT NULL,
    ttft_count bigint DEFAULT 0 NOT NULL,
    duration_sum_ms bigint DEFAULT 0 NOT NULL,
    duration_count bigint DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: channel_monitor_v2_user_metrics_rollup; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_user_metrics_rollup (
    bucket_start timestamp with time zone NOT NULL,
    bucket_seconds integer NOT NULL,
    platform text NOT NULL,
    group_id bigint DEFAULT 0 NOT NULL,
    model text NOT NULL,
    user_id bigint NOT NULL,
    success_requests bigint DEFAULT 0 NOT NULL,
    error_requests bigint DEFAULT 0 NOT NULL,
    input_tokens bigint DEFAULT 0 NOT NULL,
    output_tokens bigint DEFAULT 0 NOT NULL,
    cache_creation_tokens bigint DEFAULT 0 NOT NULL,
    cache_read_tokens bigint DEFAULT 0 NOT NULL,
    ttft_sum_ms bigint DEFAULT 0 NOT NULL,
    ttft_count bigint DEFAULT 0 NOT NULL,
    duration_sum_ms bigint DEFAULT 0 NOT NULL,
    duration_count bigint DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT channel_monitor_v2_user_metrics_rollup_bucket_seconds_check CHECK ((bucket_seconds = ANY (ARRAY[300, 3600, 43200, 86400])))
);


--
-- Name: channel_monitor_v2_watermarks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitor_v2_watermarks (
    id smallint DEFAULT 1 NOT NULL,
    usage_coverage_start timestamp with time zone,
    error_coverage_start timestamp with time zone,
    data_through timestamp with time zone,
    last_successful_at timestamp with time zone,
    backfill_cursor timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT channel_monitor_v2_watermarks_id_check CHECK ((id = 1))
);


--
-- Name: channel_monitors; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_monitors (
    id bigint NOT NULL,
    name character varying(100) NOT NULL,
    provider character varying(20) NOT NULL,
    endpoint character varying(500) NOT NULL,
    api_key_encrypted text NOT NULL,
    primary_model character varying(200) NOT NULL,
    extra_models jsonb DEFAULT '[]'::jsonb NOT NULL,
    group_name character varying(100) DEFAULT ''::character varying NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    interval_seconds integer NOT NULL,
    last_checked_at timestamp with time zone,
    created_by bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    template_id bigint,
    extra_headers jsonb DEFAULT '{}'::jsonb NOT NULL,
    body_override_mode character varying(10) DEFAULT 'off'::character varying NOT NULL,
    body_override jsonb,
    api_mode character varying(32) DEFAULT 'chat_completions'::character varying NOT NULL,
    jitter_seconds integer DEFAULT 0 NOT NULL,
    check_mode character varying(32) DEFAULT 'probe'::character varying NOT NULL,
    account_id bigint,
    CONSTRAINT channel_monitors_api_mode_check CHECK (((api_mode)::text = ANY ((ARRAY['chat_completions'::character varying, 'responses'::character varying])::text[]))),
    CONSTRAINT channel_monitors_body_mode_check CHECK (((body_override_mode)::text = ANY ((ARRAY['off'::character varying, 'merge'::character varying, 'replace'::character varying])::text[]))),
    CONSTRAINT channel_monitors_check_mode_check CHECK (((check_mode)::text = ANY ((ARRAY['probe'::character varying, 'quota'::character varying, 'quota_probe'::character varying])::text[]))),
    CONSTRAINT channel_monitors_interval_check CHECK (((interval_seconds >= 15) AND (interval_seconds <= 3600))),
    CONSTRAINT channel_monitors_provider_check CHECK (((provider)::text = ANY ((ARRAY['openai'::character varying, 'anthropic'::character varying, 'gemini'::character varying, 'grok'::character varying, 'antigravity'::character varying, 'kimi'::character varying, 'zhipu'::character varying, 'deepseek'::character varying, 'minimax'::character varying, 'opencode_go'::character varying])::text[])))
);


--
-- Name: COLUMN channel_monitors.check_mode; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_monitors.check_mode IS 'probe = LLM 探活（默认）；quota = 仅查关联账号用量；quota_probe = 探活 + 配额';


--
-- Name: COLUMN channel_monitors.account_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_monitors.account_id IS '配额模式关联的账号 ID（数据源）；账号删除时置空';


--
-- Name: channel_monitors_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_monitors_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_monitors_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_monitors_id_seq OWNED BY public.channel_monitors.id;


--
-- Name: channel_pricing_intervals; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channel_pricing_intervals (
    id bigint NOT NULL,
    pricing_id bigint NOT NULL,
    min_tokens integer DEFAULT 0 NOT NULL,
    max_tokens integer,
    tier_label character varying(50),
    input_price numeric(20,12),
    output_price numeric(20,12),
    cache_write_price numeric(20,12),
    cache_read_price numeric(20,12),
    per_request_price numeric(20,12),
    sort_order integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    input_multiplier numeric(12,6),
    output_multiplier numeric(12,6),
    cache_write_multiplier numeric(12,6),
    cache_read_multiplier numeric(12,6),
    cache_write_1h_price numeric(20,12),
    CONSTRAINT channel_pricing_intervals_cache_read_multiplier_positive CHECK (((cache_read_multiplier IS NULL) OR (cache_read_multiplier > (0)::numeric))),
    CONSTRAINT channel_pricing_intervals_cache_write_multiplier_positive CHECK (((cache_write_multiplier IS NULL) OR (cache_write_multiplier > (0)::numeric))),
    CONSTRAINT channel_pricing_intervals_input_multiplier_positive CHECK (((input_multiplier IS NULL) OR (input_multiplier > (0)::numeric))),
    CONSTRAINT channel_pricing_intervals_output_multiplier_positive CHECK (((output_multiplier IS NULL) OR (output_multiplier > (0)::numeric)))
);


--
-- Name: TABLE channel_pricing_intervals; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.channel_pricing_intervals IS '渠道定价区间：支持按 token 区间、按次分层、图片分辨率分层';


--
-- Name: COLUMN channel_pricing_intervals.min_tokens; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.min_tokens IS '区间下界（含），token 模式使用';


--
-- Name: COLUMN channel_pricing_intervals.max_tokens; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.max_tokens IS '区间上界（不含），NULL 表示无上限';


--
-- Name: COLUMN channel_pricing_intervals.tier_label; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.tier_label IS '层级标签，按次/图片模式使用（如 1K、2K、4K、HD）';


--
-- Name: COLUMN channel_pricing_intervals.input_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.input_price IS 'token 模式：每 token 输入价';


--
-- Name: COLUMN channel_pricing_intervals.output_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.output_price IS 'token 模式：每 token 输出价';


--
-- Name: COLUMN channel_pricing_intervals.cache_write_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.cache_write_price IS 'token 模式：缓存写入价';


--
-- Name: COLUMN channel_pricing_intervals.cache_read_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.cache_read_price IS 'token 模式：缓存读取价';


--
-- Name: COLUMN channel_pricing_intervals.per_request_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.per_request_price IS '按次/图片模式：每次请求价格';


--
-- Name: COLUMN channel_pricing_intervals.input_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.input_multiplier IS 'Interval input multiplier applied to the channel base input price when input_price is NULL';


--
-- Name: COLUMN channel_pricing_intervals.output_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.output_multiplier IS 'Interval output multiplier applied to the channel base output price when output_price is NULL';


--
-- Name: COLUMN channel_pricing_intervals.cache_write_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.cache_write_multiplier IS 'Interval cache-write multiplier applied to the channel base cache-write price when cache_write_price is NULL';


--
-- Name: COLUMN channel_pricing_intervals.cache_read_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.cache_read_multiplier IS 'Interval cache-read multiplier applied to the channel base cache-read price when cache_read_price is NULL';


--
-- Name: COLUMN channel_pricing_intervals.cache_write_1h_price; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channel_pricing_intervals.cache_write_1h_price IS 'Interval-specific 1h cache write price per token';


--
-- Name: channel_pricing_intervals_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channel_pricing_intervals_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channel_pricing_intervals_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channel_pricing_intervals_id_seq OWNED BY public.channel_pricing_intervals.id;


--
-- Name: channels; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.channels (
    id bigint NOT NULL,
    name character varying(100) NOT NULL,
    description text DEFAULT ''::text,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    model_mapping jsonb DEFAULT '{}'::jsonb,
    billing_model_source character varying(20) DEFAULT 'channel_mapped'::character varying,
    restrict_models boolean DEFAULT false,
    features text DEFAULT ''::text NOT NULL,
    apply_pricing_to_account_stats boolean DEFAULT false NOT NULL,
    features_config jsonb DEFAULT '{}'::jsonb NOT NULL
);


--
-- Name: TABLE channels; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.channels IS '渠道管理：关联多个分组，提供自定义模型定价';


--
-- Name: COLUMN channels.model_mapping; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channels.model_mapping IS '渠道级模型映射，在账号映射之前执行。格式：{"source_model": "target_model"}';


--
-- Name: COLUMN channels.features; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channels.features IS '渠道特性描述，JSON 数组格式，用于支付页面展示';


--
-- Name: COLUMN channels.features_config; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.channels.features_config IS '渠道特性配置（如 web_search_emulation），JSON 对象格式';


--
-- Name: channels_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.channels_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: channels_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.channels_id_seq OWNED BY public.channels.id;


--
-- Name: composite_model_routes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.composite_model_routes (
    id bigint NOT NULL,
    group_id bigint NOT NULL,
    public_model character varying(200) NOT NULL,
    match_type character varying(20) DEFAULT 'exact'::character varying NOT NULL,
    target_platform character varying(50) NOT NULL,
    upstream_model character varying(200) DEFAULT ''::character varying NOT NULL,
    endpoint character varying(50) DEFAULT 'any'::character varying NOT NULL,
    priority integer DEFAULT 100 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    notes text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT composite_model_routes_endpoint_check CHECK (((endpoint)::text = ANY ((ARRAY['any'::character varying, 'messages'::character varying, 'count_tokens'::character varying, 'responses'::character varying, 'chat_completions'::character varying, 'embeddings'::character varying, 'images'::character varying, 'gemini'::character varying])::text[]))),
    CONSTRAINT composite_model_routes_match_type_check CHECK (((match_type)::text = ANY ((ARRAY['exact'::character varying, 'prefix'::character varying])::text[]))),
    CONSTRAINT composite_model_routes_target_platform_check CHECK (((target_platform)::text = ANY ((ARRAY['anthropic'::character varying, 'openai'::character varying, 'gemini'::character varying, 'antigravity'::character varying, 'grok'::character varying, 'kimi'::character varying, 'zhipu'::character varying, 'deepseek'::character varying, 'minimax'::character varying, 'opencode_go'::character varying])::text[])))
);


--
-- Name: composite_model_routes_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.composite_model_routes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: composite_model_routes_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.composite_model_routes_id_seq OWNED BY public.composite_model_routes.id;


--
-- Name: content_moderation_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.content_moderation_logs (
    id bigint NOT NULL,
    request_id character varying(128) DEFAULT ''::character varying NOT NULL,
    user_id bigint,
    user_email character varying(255) DEFAULT ''::character varying NOT NULL,
    api_key_id bigint,
    api_key_name character varying(100) DEFAULT ''::character varying NOT NULL,
    group_id bigint,
    group_name character varying(255) DEFAULT ''::character varying NOT NULL,
    endpoint character varying(128) DEFAULT ''::character varying NOT NULL,
    provider character varying(64) DEFAULT ''::character varying NOT NULL,
    model character varying(255) DEFAULT ''::character varying NOT NULL,
    mode character varying(32) DEFAULT ''::character varying NOT NULL,
    action character varying(32) DEFAULT ''::character varying NOT NULL,
    flagged boolean DEFAULT false NOT NULL,
    highest_category character varying(64) DEFAULT ''::character varying NOT NULL,
    highest_score numeric(8,6) DEFAULT 0 NOT NULL,
    category_scores jsonb DEFAULT '{}'::jsonb NOT NULL,
    threshold_snapshot jsonb DEFAULT '{}'::jsonb NOT NULL,
    input_excerpt text DEFAULT ''::text NOT NULL,
    upstream_latency_ms integer,
    error text DEFAULT ''::text NOT NULL,
    violation_count integer DEFAULT 0 NOT NULL,
    auto_banned boolean DEFAULT false NOT NULL,
    email_sent boolean DEFAULT false NOT NULL,
    queue_delay_ms integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    matched_keyword character varying(255) DEFAULT ''::character varying NOT NULL,
    engine_meta jsonb
);


--
-- Name: content_moderation_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.content_moderation_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: content_moderation_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.content_moderation_logs_id_seq OWNED BY public.content_moderation_logs.id;


--
-- Name: deleted_api_key_audits; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deleted_api_key_audits (
    id bigint NOT NULL,
    key character varying(128) NOT NULL,
    api_key_id bigint NOT NULL,
    user_id bigint NOT NULL,
    key_name character varying(100) DEFAULT ''::character varying NOT NULL,
    deleted_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: deleted_api_key_audits_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.deleted_api_key_audits_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: deleted_api_key_audits_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.deleted_api_key_audits_id_seq OWNED BY public.deleted_api_key_audits.id;


--
-- Name: error_passthrough_rules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.error_passthrough_rules (
    id bigint NOT NULL,
    name character varying(100) NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    error_codes jsonb DEFAULT '[]'::jsonb,
    keywords jsonb DEFAULT '[]'::jsonb,
    match_mode character varying(10) DEFAULT 'any'::character varying NOT NULL,
    platforms jsonb DEFAULT '[]'::jsonb,
    passthrough_code boolean DEFAULT true NOT NULL,
    response_code integer,
    passthrough_body boolean DEFAULT true NOT NULL,
    custom_message text,
    description text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    skip_monitoring boolean DEFAULT false NOT NULL
);


--
-- Name: error_passthrough_rules_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.error_passthrough_rules_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: error_passthrough_rules_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.error_passthrough_rules_id_seq OWNED BY public.error_passthrough_rules.id;


--
-- Name: groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.groups (
    id bigint NOT NULL,
    name character varying(100) NOT NULL,
    description text,
    rate_multiplier numeric(10,4) DEFAULT 1.0 NOT NULL,
    is_exclusive boolean DEFAULT false NOT NULL,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    platform character varying(50) DEFAULT 'anthropic'::character varying NOT NULL,
    subscription_type character varying(20) DEFAULT 'standard'::character varying NOT NULL,
    daily_limit_usd numeric(20,8) DEFAULT NULL::numeric,
    weekly_limit_usd numeric(20,8) DEFAULT NULL::numeric,
    monthly_limit_usd numeric(20,8) DEFAULT NULL::numeric,
    default_validity_days integer DEFAULT 30 NOT NULL,
    image_price_1k numeric(20,8),
    image_price_2k numeric(20,8),
    image_price_4k numeric(20,8),
    claude_code_only boolean DEFAULT false NOT NULL,
    fallback_group_id bigint,
    model_routing jsonb DEFAULT '{}'::jsonb,
    model_routing_enabled boolean DEFAULT false NOT NULL,
    fallback_group_id_on_invalid_request bigint,
    mcp_xml_inject boolean DEFAULT true NOT NULL,
    supported_model_scopes jsonb DEFAULT '["claude", "gemini_text", "gemini_image"]'::jsonb NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    allow_messages_dispatch boolean DEFAULT false NOT NULL,
    default_mapped_model character varying(100) DEFAULT ''::character varying NOT NULL,
    require_oauth_only boolean DEFAULT false NOT NULL,
    require_privacy_set boolean DEFAULT false NOT NULL,
    messages_dispatch_model_config jsonb DEFAULT '{}'::jsonb NOT NULL,
    rpm_limit integer DEFAULT 0 NOT NULL,
    allow_image_generation boolean DEFAULT false NOT NULL,
    image_rate_independent boolean DEFAULT false NOT NULL,
    image_rate_multiplier numeric(10,4) DEFAULT 1.0 NOT NULL,
    model_allowlist jsonb DEFAULT '{}'::jsonb NOT NULL,
    peak_rate_enabled boolean DEFAULT false NOT NULL,
    peak_start character varying(5) DEFAULT ''::character varying NOT NULL,
    peak_end character varying(5) DEFAULT ''::character varying NOT NULL,
    peak_rate_multiplier numeric(10,4) DEFAULT 1.0 NOT NULL,
    batch_image_discount_multiplier numeric(10,4) DEFAULT 0.5 NOT NULL,
    batch_image_hold_multiplier numeric(10,4) DEFAULT 0.6 NOT NULL,
    allow_batch_image_generation boolean DEFAULT false NOT NULL,
    video_rate_independent boolean DEFAULT false NOT NULL,
    video_rate_multiplier numeric(10,4) DEFAULT 1.0 NOT NULL,
    video_price_480p numeric(20,8),
    video_price_720p numeric(20,8),
    video_price_1080p numeric(20,8),
    web_search_price_per_call numeric(20,8),
    duplicate_operation_id character varying(64),
    max_reasoning_effort character varying(20) DEFAULT ''::character varying NOT NULL,
    reasoning_effort_mappings jsonb DEFAULT '[]'::jsonb NOT NULL,
    allow_live boolean DEFAULT false NOT NULL,
    profit_control_enabled boolean DEFAULT false NOT NULL,
    profit_min_margin numeric(10,4) DEFAULT 0 NOT NULL,
    profit_safety_buffer numeric(10,4) DEFAULT 0 NOT NULL,
    video_model_prices jsonb,
    audio_realtime_price_per_min numeric(20,8),
    audio_tts_price_per_million_chars numeric(20,8),
    audio_stt_price_per_hour numeric(20,8),
    search_price_per_1k numeric(20,8),
    long_context_pricing_enabled boolean DEFAULT true NOT NULL,
    model_pricing jsonb,
    force_openai_fast boolean DEFAULT false NOT NULL,
    max_reasoning_effort_over_limit character varying(20) DEFAULT 'downgrade'::character varying NOT NULL,
    free_openai_fast boolean DEFAULT false NOT NULL,
    codex_models_manifest_config jsonb DEFAULT '{}'::jsonb NOT NULL
);


--
-- Name: COLUMN groups.image_price_1k; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.image_price_1k IS '1K 分辨率图片生成单价 (USD)，仅 antigravity 平台使用';


--
-- Name: COLUMN groups.image_price_2k; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.image_price_2k IS '2K 分辨率图片生成单价 (USD)，仅 antigravity 平台使用';


--
-- Name: COLUMN groups.image_price_4k; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.image_price_4k IS '4K 分辨率图片生成单价 (USD)，仅 antigravity 平台使用';


--
-- Name: COLUMN groups.claude_code_only; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.claude_code_only IS '是否仅允许 Claude Code 客户端访问此分组';


--
-- Name: COLUMN groups.fallback_group_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.fallback_group_id IS '非 Claude Code 请求降级使用的分组 ID';


--
-- Name: COLUMN groups.model_routing; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.model_routing IS '模型路由配置：{"model_pattern": [account_id1, account_id2], ...}，支持通配符匹配';


--
-- Name: COLUMN groups.fallback_group_id_on_invalid_request; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.fallback_group_id_on_invalid_request IS '无效请求兜底使用的分组 ID';


--
-- Name: COLUMN groups.supported_model_scopes; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.supported_model_scopes IS '支持的模型系列：claude, gemini_text, gemini_image';


--
-- Name: COLUMN groups.rpm_limit; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.rpm_limit IS '分组 RPM 上限；0 表示不限制；设置后接管该分组用户的限流（覆盖用户级 rpm_limit）。';


--
-- Name: COLUMN groups.allow_image_generation; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.allow_image_generation IS '是否允许该分组使用图片生成能力';


--
-- Name: COLUMN groups.image_rate_independent; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.image_rate_independent IS '图片生成是否使用独立倍率；false 表示共享分组有效倍率';


--
-- Name: COLUMN groups.image_rate_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.image_rate_multiplier IS '图片生成独立倍率，仅 image_rate_independent=true 时生效';


--
-- Name: COLUMN groups.model_allowlist; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.model_allowlist IS 'Group model allowlist: constrains both model listing responses and request admission';


--
-- Name: COLUMN groups.batch_image_discount_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.batch_image_discount_multiplier IS '批量图片生成折扣倍率，最终单价会乘以该值；0 表示免费';


--
-- Name: COLUMN groups.batch_image_hold_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.batch_image_hold_multiplier IS '批量图片生成冻结价格比例，按普通生图原价乘以该比例冻结，结算后释放差额';


--
-- Name: COLUMN groups.allow_batch_image_generation; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.allow_batch_image_generation IS '是否允许该分组使用批量图片生成能力';


--
-- Name: COLUMN groups.video_rate_independent; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.video_rate_independent IS '视频生成是否使用独立倍率；false 表示共享分组有效倍率';


--
-- Name: COLUMN groups.video_rate_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.video_rate_multiplier IS '视频生成独立倍率，仅 video_rate_independent=true 时生效';


--
-- Name: COLUMN groups.video_price_480p; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.video_price_480p IS '480p 视频生成每秒单价 (USD/s)，Grok 平台使用';


--
-- Name: COLUMN groups.video_price_720p; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.video_price_720p IS '720p 视频生成每秒单价 (USD/s)，Grok 平台使用';


--
-- Name: COLUMN groups.video_price_1080p; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.video_price_1080p IS '1080p 视频生成每秒单价 (USD/s)，Grok 平台使用';


--
-- Name: COLUMN groups.video_model_prices; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.video_model_prices IS '可选：按模型族×分辨率覆盖视频每秒单价 (USD/s)。key 为规范模型族 (grok-imagine-video / grok-imagine-video-1.5)，value 为分辨率→单价映射；NULL/空表示不覆盖，回退到 video_price_* 列或官方默认';


--
-- Name: COLUMN groups.long_context_pricing_enabled; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.long_context_pricing_enabled IS 'Whether token pricing selects official/preset long-context tiers; default true preserves existing long-context billing';


--
-- Name: COLUMN groups.model_pricing; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.model_pricing IS 'Per-model group pricing overrides channel and built-in model pricing';


--
-- Name: COLUMN groups.force_openai_fast; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.force_openai_fast IS 'Force OpenAI gateway requests in this group to use service_tier=priority before global Fast/Flex policy evaluation';


--
-- Name: COLUMN groups.free_openai_fast; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.free_openai_fast IS 'Whether Fast/priority requests in this OpenAI/Composite group are billed to users at Standard price';


--
-- Name: COLUMN groups.codex_models_manifest_config; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.groups.codex_models_manifest_config IS 'Pinned-accounts Codex models manifest config for OpenAI groups: {"enabled":bool,"account_ids":[int64],"fallback_to_scheduler":bool}; when enabled the Codex /models manifest is fetched only from the pinned accounts';


--
-- Name: groups_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.groups_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: groups_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.groups_id_seq OWNED BY public.groups.id;


--
-- Name: groups_video_price_backup_220; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.groups_video_price_backup_220 (
    group_id bigint,
    platform character varying(50),
    video_price_480p numeric(20,8),
    video_price_720p numeric(20,8),
    video_price_1080p numeric(20,8),
    video_model_prices jsonb,
    backed_up_at timestamp with time zone
);


--
-- Name: TABLE groups_video_price_backup_220; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.groups_video_price_backup_220 IS '迁移 220 清空非 Grok/非 composite 分组视频价前的快照。composite 可能路由到 Grok 账号，予以保留。确认无需回滚后可安全 DROP；回滚方式：UPDATE groups g SET video_price_480p = b.video_price_480p, ... FROM groups_video_price_backup_220 b WHERE g.id = b.group_id';


--
-- Name: idempotency_records; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.idempotency_records (
    id bigint NOT NULL,
    scope character varying(128) NOT NULL,
    idempotency_key_hash character varying(64) NOT NULL,
    request_fingerprint character varying(64) NOT NULL,
    status character varying(32) NOT NULL,
    response_status integer,
    response_body text,
    error_reason character varying(128),
    locked_until timestamp with time zone,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: idempotency_records_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.idempotency_records_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: idempotency_records_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.idempotency_records_id_seq OWNED BY public.idempotency_records.id;


--
-- Name: identity_adoption_decisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.identity_adoption_decisions (
    id bigint NOT NULL,
    pending_auth_session_id bigint NOT NULL,
    identity_id bigint,
    adopt_display_name boolean DEFAULT false NOT NULL,
    adopt_avatar boolean DEFAULT false NOT NULL,
    decided_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: identity_adoption_decisions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.identity_adoption_decisions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: identity_adoption_decisions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.identity_adoption_decisions_id_seq OWNED BY public.identity_adoption_decisions.id;


--
-- Name: ops_alert_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_alert_events (
    id bigint NOT NULL,
    rule_id bigint,
    severity character varying(16) NOT NULL,
    status character varying(16) DEFAULT 'firing'::character varying NOT NULL,
    title character varying(200),
    description text,
    metric_value double precision,
    threshold_value double precision,
    dimensions jsonb,
    fired_at timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    email_sent boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: ops_alert_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_alert_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_alert_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_alert_events_id_seq OWNED BY public.ops_alert_events.id;


--
-- Name: ops_alert_rules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_alert_rules (
    id bigint NOT NULL,
    name character varying(128) NOT NULL,
    description text,
    enabled boolean DEFAULT true NOT NULL,
    severity character varying(16) DEFAULT 'warning'::character varying NOT NULL,
    metric_type character varying(64) NOT NULL,
    operator character varying(8) NOT NULL,
    threshold double precision NOT NULL,
    window_minutes integer DEFAULT 5 NOT NULL,
    sustained_minutes integer DEFAULT 5 NOT NULL,
    cooldown_minutes integer DEFAULT 10 NOT NULL,
    filters jsonb,
    last_triggered_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    notify_email boolean DEFAULT true NOT NULL
);


--
-- Name: ops_alert_rules_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_alert_rules_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_alert_rules_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_alert_rules_id_seq OWNED BY public.ops_alert_rules.id;


--
-- Name: ops_error_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_error_logs (
    id bigint NOT NULL,
    request_id character varying(64),
    client_request_id character varying(64),
    user_id bigint,
    api_key_id bigint,
    account_id bigint,
    group_id bigint,
    client_ip inet,
    platform character varying(32),
    model character varying(100),
    request_path character varying(256),
    stream boolean DEFAULT false NOT NULL,
    user_agent text,
    error_phase character varying(32) NOT NULL,
    error_type character varying(64) NOT NULL,
    severity character varying(8) DEFAULT 'P2'::character varying NOT NULL,
    status_code integer,
    is_business_limited boolean DEFAULT false NOT NULL,
    error_message text,
    error_body text,
    error_source character varying(64),
    error_owner character varying(32),
    account_status character varying(50),
    upstream_status_code integer,
    upstream_error_message text,
    upstream_error_detail text,
    provider_error_code character varying(64),
    provider_error_type character varying(64),
    network_error_type character varying(50),
    retry_after_seconds integer,
    duration_ms integer,
    time_to_first_token_ms bigint,
    auth_latency_ms bigint,
    routing_latency_ms bigint,
    upstream_latency_ms bigint,
    response_latency_ms bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    upstream_errors jsonb,
    is_count_tokens boolean DEFAULT false NOT NULL,
    resolved boolean DEFAULT false NOT NULL,
    resolved_at timestamp with time zone,
    resolved_by_user_id bigint,
    inbound_endpoint character varying(256),
    upstream_endpoint character varying(256),
    requested_model character varying(100),
    upstream_model character varying(100),
    request_type smallint,
    attempted_key_prefix character varying(32),
    deleted_key_owner_user_id bigint,
    deleted_key_name character varying(100),
    api_key_prefix character varying(32)
);


--
-- Name: TABLE ops_error_logs; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.ops_error_logs IS 'Ops error logs (vNext). Stores sanitized error details; request replay storage removed.';


--
-- Name: COLUMN ops_error_logs.upstream_errors; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_error_logs.upstream_errors IS 'Sanitized upstream error events list (JSON array), correlated per gateway request (request_id/client_request_id); used for per-request upstream debugging.';


--
-- Name: COLUMN ops_error_logs.is_count_tokens; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_error_logs.is_count_tokens IS '是否为 count_tokens 请求的错误（用于统计过滤）';


--
-- Name: COLUMN ops_error_logs.inbound_endpoint; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_error_logs.inbound_endpoint IS 'Normalized client-facing API endpoint path, e.g. /v1/chat/completions. Populated from InboundEndpointMiddleware.';


--
-- Name: COLUMN ops_error_logs.upstream_endpoint; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_error_logs.upstream_endpoint IS 'Normalized upstream endpoint path derived from platform, e.g. /v1/responses.';


--
-- Name: COLUMN ops_error_logs.requested_model; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_error_logs.requested_model IS 'Client-requested model name before mapping (raw from request body).';


--
-- Name: COLUMN ops_error_logs.upstream_model; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_error_logs.upstream_model IS 'Actual model sent to upstream provider after mapping. NULL means no mapping applied.';


--
-- Name: COLUMN ops_error_logs.request_type; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_error_logs.request_type IS 'Request type enum: 0=unknown, 1=sync, 2=stream, 3=ws_v2. Matches usage_logs.request_type semantics.';


--
-- Name: ops_error_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_error_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_error_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_error_logs_id_seq OWNED BY public.ops_error_logs.id;


--
-- Name: ops_ingress_reject_aggregates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_ingress_reject_aggregates (
    id bigint NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    reject_reason character varying(64) NOT NULL,
    route_family character varying(64) NOT NULL,
    protocol character varying(32) NOT NULL,
    client_ip inet NOT NULL,
    user_id bigint DEFAULT 0 NOT NULL,
    api_key_id bigint DEFAULT 0 NOT NULL,
    request_count bigint DEFAULT 0 NOT NULL,
    first_seen timestamp with time zone NOT NULL,
    last_seen timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: ops_ingress_reject_aggregates_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_ingress_reject_aggregates_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_ingress_reject_aggregates_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_ingress_reject_aggregates_id_seq OWNED BY public.ops_ingress_reject_aggregates.id;


--
-- Name: ops_job_heartbeats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_job_heartbeats (
    job_name character varying(64) NOT NULL,
    last_run_at timestamp with time zone,
    last_success_at timestamp with time zone,
    last_error_at timestamp with time zone,
    last_error text,
    last_duration_ms bigint,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_result text
);


--
-- Name: TABLE ops_job_heartbeats; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.ops_job_heartbeats IS 'Ops background jobs heartbeats (vNext).';


--
-- Name: COLUMN ops_job_heartbeats.last_result; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_job_heartbeats.last_result IS 'Last successful run result summary (human readable).';


--
-- Name: ops_metrics_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_metrics_daily (
    id bigint NOT NULL,
    bucket_date date NOT NULL,
    platform character varying(32),
    group_id bigint,
    success_count bigint DEFAULT 0 NOT NULL,
    error_count_total bigint DEFAULT 0 NOT NULL,
    business_limited_count bigint DEFAULT 0 NOT NULL,
    error_count_sla bigint DEFAULT 0 NOT NULL,
    upstream_error_count_excl_429_529 bigint DEFAULT 0 NOT NULL,
    upstream_429_count bigint DEFAULT 0 NOT NULL,
    upstream_529_count bigint DEFAULT 0 NOT NULL,
    token_consumed bigint DEFAULT 0 NOT NULL,
    duration_p50_ms integer,
    duration_p90_ms integer,
    duration_p95_ms integer,
    duration_p99_ms integer,
    ttft_p50_ms integer,
    ttft_p90_ms integer,
    ttft_p95_ms integer,
    ttft_p99_ms integer,
    computed_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    duration_avg_ms double precision,
    duration_max_ms integer,
    ttft_avg_ms double precision,
    ttft_max_ms integer,
    ttft_sample_count bigint DEFAULT 0 NOT NULL
);


--
-- Name: TABLE ops_metrics_daily; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.ops_metrics_daily IS 'vNext daily pre-aggregated ops metrics (overall/platform/group).';


--
-- Name: ops_metrics_daily_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_metrics_daily_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_metrics_daily_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_metrics_daily_id_seq OWNED BY public.ops_metrics_daily.id;


--
-- Name: ops_metrics_hourly; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_metrics_hourly (
    id bigint NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    platform character varying(32),
    group_id bigint,
    success_count bigint DEFAULT 0 NOT NULL,
    error_count_total bigint DEFAULT 0 NOT NULL,
    business_limited_count bigint DEFAULT 0 NOT NULL,
    error_count_sla bigint DEFAULT 0 NOT NULL,
    upstream_error_count_excl_429_529 bigint DEFAULT 0 NOT NULL,
    upstream_429_count bigint DEFAULT 0 NOT NULL,
    upstream_529_count bigint DEFAULT 0 NOT NULL,
    token_consumed bigint DEFAULT 0 NOT NULL,
    duration_p50_ms integer,
    duration_p90_ms integer,
    duration_p95_ms integer,
    duration_p99_ms integer,
    ttft_p50_ms integer,
    ttft_p90_ms integer,
    ttft_p95_ms integer,
    ttft_p99_ms integer,
    computed_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    duration_avg_ms double precision,
    duration_max_ms integer,
    ttft_avg_ms double precision,
    ttft_max_ms integer,
    ttft_sample_count bigint DEFAULT 0 NOT NULL
);


--
-- Name: TABLE ops_metrics_hourly; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.ops_metrics_hourly IS 'vNext hourly pre-aggregated ops metrics (overall/platform/group).';


--
-- Name: ops_metrics_hourly_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_metrics_hourly_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_metrics_hourly_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_metrics_hourly_id_seq OWNED BY public.ops_metrics_hourly.id;


--
-- Name: ops_system_log_cleanup_audits; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_system_log_cleanup_audits (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    operator_id bigint NOT NULL,
    conditions jsonb DEFAULT '{}'::jsonb NOT NULL,
    deleted_rows bigint DEFAULT 0 NOT NULL
);


--
-- Name: ops_system_log_cleanup_audits_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_system_log_cleanup_audits_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_system_log_cleanup_audits_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_system_log_cleanup_audits_id_seq OWNED BY public.ops_system_log_cleanup_audits.id;


--
-- Name: ops_system_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_system_logs (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    level character varying(16) NOT NULL,
    component character varying(128) DEFAULT ''::character varying NOT NULL,
    message text NOT NULL,
    request_id character varying(128),
    client_request_id character varying(128),
    user_id bigint,
    account_id bigint,
    platform character varying(32),
    model character varying(128),
    extra jsonb DEFAULT '{}'::jsonb NOT NULL,
    api_key_id bigint,
    host character varying(255)
);


--
-- Name: ops_system_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_system_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_system_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_system_logs_id_seq OWNED BY public.ops_system_logs.id;


--
-- Name: ops_system_metrics; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ops_system_metrics (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    window_minutes integer DEFAULT 1 NOT NULL,
    platform character varying(32),
    group_id bigint,
    success_count bigint DEFAULT 0 NOT NULL,
    error_count_total bigint DEFAULT 0 NOT NULL,
    business_limited_count bigint DEFAULT 0 NOT NULL,
    error_count_sla bigint DEFAULT 0 NOT NULL,
    upstream_error_count_excl_429_529 bigint DEFAULT 0 NOT NULL,
    upstream_429_count bigint DEFAULT 0 NOT NULL,
    upstream_529_count bigint DEFAULT 0 NOT NULL,
    token_consumed bigint DEFAULT 0 NOT NULL,
    qps double precision,
    tps double precision,
    duration_p50_ms integer,
    duration_p90_ms integer,
    duration_p95_ms integer,
    duration_p99_ms integer,
    duration_avg_ms double precision,
    duration_max_ms integer,
    ttft_p50_ms integer,
    ttft_p90_ms integer,
    ttft_p95_ms integer,
    ttft_p99_ms integer,
    ttft_avg_ms double precision,
    ttft_max_ms integer,
    cpu_usage_percent double precision,
    memory_used_mb bigint,
    memory_total_mb bigint,
    memory_usage_percent double precision,
    db_ok boolean,
    redis_ok boolean,
    db_conn_active integer,
    db_conn_idle integer,
    db_conn_waiting integer,
    goroutine_count integer,
    concurrency_queue_depth integer,
    redis_conn_total integer,
    redis_conn_idle integer,
    account_switch_count bigint DEFAULT 0 NOT NULL
);


--
-- Name: TABLE ops_system_metrics; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.ops_system_metrics IS 'Ops system/request metrics snapshots (vNext). Used for dashboard overview and realtime rates.';


--
-- Name: COLUMN ops_system_metrics.redis_conn_total; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_system_metrics.redis_conn_total IS 'Redis pool total connections (go-redis PoolStats.TotalConns).';


--
-- Name: COLUMN ops_system_metrics.redis_conn_idle; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.ops_system_metrics.redis_conn_idle IS 'Redis pool idle connections (go-redis PoolStats.IdleConns).';


--
-- Name: ops_system_metrics_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ops_system_metrics_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ops_system_metrics_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ops_system_metrics_id_seq OWNED BY public.ops_system_metrics.id;


--
-- Name: orphan_allowed_groups_audit; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.orphan_allowed_groups_audit (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    group_id bigint NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE orphan_allowed_groups_audit; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.orphan_allowed_groups_audit IS '审计表：记录 users.allowed_groups 中引用的不存在的 group_id，用于数据清理前的审计';


--
-- Name: orphan_allowed_groups_audit_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.orphan_allowed_groups_audit_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: orphan_allowed_groups_audit_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.orphan_allowed_groups_audit_id_seq OWNED BY public.orphan_allowed_groups_audit.id;


--
-- Name: passkey_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.passkey_credentials (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    credential_id bytea NOT NULL,
    name character varying(100) DEFAULT 'Passkey'::character varying NOT NULL,
    credential_data jsonb NOT NULL,
    last_used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: passkey_credentials_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.passkey_credentials_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: passkey_credentials_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.passkey_credentials_id_seq OWNED BY public.passkey_credentials.id;


--
-- Name: passkey_user_handles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.passkey_user_handles (
    user_id bigint NOT NULL,
    user_handle bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: payment_audit_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.payment_audit_logs (
    id bigint NOT NULL,
    order_id character varying(64) NOT NULL,
    action character varying(50) NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    operator character varying(100) DEFAULT 'system'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: payment_audit_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.payment_audit_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: payment_audit_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.payment_audit_logs_id_seq OWNED BY public.payment_audit_logs.id;


--
-- Name: payment_orders; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.payment_orders (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    user_email character varying(255) DEFAULT ''::character varying NOT NULL,
    user_name character varying(100) DEFAULT ''::character varying NOT NULL,
    user_notes text,
    amount numeric(20,2) NOT NULL,
    pay_amount numeric(20,2) NOT NULL,
    fee_rate numeric(10,4) DEFAULT 0 NOT NULL,
    recharge_code character varying(64) DEFAULT ''::character varying NOT NULL,
    payment_type character varying(30) DEFAULT ''::character varying NOT NULL,
    payment_trade_no character varying(128) DEFAULT ''::character varying NOT NULL,
    pay_url text,
    qr_code text,
    qr_code_img text,
    order_type character varying(20) DEFAULT 'balance'::character varying NOT NULL,
    plan_id bigint,
    subscription_group_id bigint,
    subscription_days integer,
    provider_instance_id character varying(64),
    status character varying(30) DEFAULT 'PENDING'::character varying NOT NULL,
    refund_amount numeric(20,2) DEFAULT 0 NOT NULL,
    refund_reason text,
    refund_at timestamp with time zone,
    force_refund boolean DEFAULT false NOT NULL,
    refund_requested_at timestamp with time zone,
    refund_request_reason text,
    refund_requested_by character varying(20),
    expires_at timestamp with time zone NOT NULL,
    paid_at timestamp with time zone,
    completed_at timestamp with time zone,
    failed_at timestamp with time zone,
    failed_reason text,
    client_ip character varying(50) DEFAULT ''::character varying NOT NULL,
    src_host character varying(255) DEFAULT ''::character varying NOT NULL,
    src_url text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    out_trade_no character varying(64) DEFAULT ''::character varying NOT NULL,
    provider_key character varying(30),
    provider_snapshot jsonb
);


--
-- Name: payment_orders_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.payment_orders_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: payment_orders_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.payment_orders_id_seq OWNED BY public.payment_orders.id;


--
-- Name: payment_provider_instances; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.payment_provider_instances (
    id bigint NOT NULL,
    provider_key character varying(30) NOT NULL,
    name character varying(100) DEFAULT ''::character varying NOT NULL,
    config text NOT NULL,
    supported_types character varying(200) DEFAULT ''::character varying NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    limits text DEFAULT ''::text NOT NULL,
    refund_enabled boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    payment_mode character varying(20) DEFAULT ''::character varying NOT NULL,
    allow_user_refund boolean DEFAULT false NOT NULL
);


--
-- Name: payment_provider_instances_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.payment_provider_instances_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: payment_provider_instances_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.payment_provider_instances_id_seq OWNED BY public.payment_provider_instances.id;


--
-- Name: pending_auth_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pending_auth_sessions (
    id bigint NOT NULL,
    session_token character varying(255) NOT NULL,
    intent character varying(40) NOT NULL,
    provider_type character varying(20) NOT NULL,
    provider_key text NOT NULL,
    provider_subject text NOT NULL,
    target_user_id bigint,
    redirect_to text DEFAULT ''::text NOT NULL,
    resolved_email text DEFAULT ''::text NOT NULL,
    registration_password_hash text DEFAULT ''::text NOT NULL,
    upstream_identity_claims jsonb DEFAULT '{}'::jsonb NOT NULL,
    local_flow_state jsonb DEFAULT '{}'::jsonb NOT NULL,
    browser_session_key text DEFAULT ''::text NOT NULL,
    completion_code_hash text DEFAULT ''::text NOT NULL,
    completion_code_expires_at timestamp with time zone,
    email_verified_at timestamp with time zone,
    password_verified_at timestamp with time zone,
    totp_verified_at timestamp with time zone,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT pending_auth_sessions_intent_check CHECK (((intent)::text = ANY ((ARRAY['login'::character varying, 'bind_current_user'::character varying, 'adopt_existing_user_by_email'::character varying])::text[]))),
    CONSTRAINT pending_auth_sessions_provider_type_check CHECK (((provider_type)::text = ANY ((ARRAY['email'::character varying, 'linuxdo'::character varying, 'wechat'::character varying, 'oidc'::character varying, 'github'::character varying, 'google'::character varying, 'dingtalk'::character varying])::text[])))
);


--
-- Name: pending_auth_sessions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.pending_auth_sessions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: pending_auth_sessions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.pending_auth_sessions_id_seq OWNED BY public.pending_auth_sessions.id;


--
-- Name: promo_code_usages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.promo_code_usages (
    id bigint NOT NULL,
    promo_code_id bigint NOT NULL,
    user_id bigint NOT NULL,
    bonus_amount numeric(20,8) NOT NULL,
    used_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE promo_code_usages; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.promo_code_usages IS '优惠码使用记录';


--
-- Name: promo_code_usages_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.promo_code_usages_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: promo_code_usages_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.promo_code_usages_id_seq OWNED BY public.promo_code_usages.id;


--
-- Name: promo_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.promo_codes (
    id bigint NOT NULL,
    code character varying(32) NOT NULL,
    bonus_amount numeric(20,8) DEFAULT 0 NOT NULL,
    max_uses integer DEFAULT 0 NOT NULL,
    used_count integer DEFAULT 0 NOT NULL,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    expires_at timestamp with time zone,
    notes text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE promo_codes; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.promo_codes IS '注册优惠码';


--
-- Name: COLUMN promo_codes.max_uses; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.promo_codes.max_uses IS '最大使用次数，0表示无限制';


--
-- Name: COLUMN promo_codes.status; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.promo_codes.status IS '状态: active, disabled';


--
-- Name: promo_codes_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.promo_codes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: promo_codes_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.promo_codes_id_seq OWNED BY public.promo_codes.id;


--
-- Name: prompt_audit_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.prompt_audit_events (
    id bigint NOT NULL,
    job_id bigint NOT NULL,
    request_id character varying(128) DEFAULT ''::character varying NOT NULL,
    user_id bigint,
    username_snapshot character varying(255) DEFAULT ''::character varying NOT NULL,
    user_email_snapshot character varying(320) DEFAULT ''::character varying NOT NULL,
    api_key_id bigint,
    api_key_name_snapshot character varying(255) DEFAULT ''::character varying NOT NULL,
    group_id bigint,
    group_name character varying(255) DEFAULT ''::character varying NOT NULL,
    provider character varying(64) DEFAULT ''::character varying NOT NULL,
    endpoint character varying(128) DEFAULT ''::character varying NOT NULL,
    protocol character varying(64) DEFAULT ''::character varying NOT NULL,
    model character varying(255) DEFAULT ''::character varying NOT NULL,
    prompt_hash character varying(64) DEFAULT ''::character varying NOT NULL,
    redacted_preview text DEFAULT ''::text NOT NULL,
    stage character varying(32) DEFAULT 'http'::character varying NOT NULL,
    decision character varying(32) DEFAULT 'pass'::character varying NOT NULL,
    risk_level character varying(32) DEFAULT 'low'::character varying NOT NULL,
    action character varying(32) DEFAULT 'Allow'::character varying NOT NULL,
    categories jsonb DEFAULT '[]'::jsonb NOT NULL,
    matched_scanners jsonb DEFAULT '[]'::jsonb NOT NULL,
    scanner_scores jsonb DEFAULT '{}'::jsonb NOT NULL,
    scanner_evidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    scanner_backend character varying(64) DEFAULT 'qwen3guard-openai'::character varying NOT NULL,
    scanner_version character varying(128) DEFAULT ''::character varying NOT NULL,
    guard_endpoint_id character varying(128) DEFAULT ''::character varying NOT NULL,
    policy_id character varying(128) DEFAULT ''::character varying NOT NULL,
    policy_version integer DEFAULT 0 NOT NULL,
    config_version bigint DEFAULT 1 NOT NULL,
    chunk_total integer DEFAULT 0 NOT NULL,
    latency_ms integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    full_prompt text DEFAULT ''::text NOT NULL,
    CONSTRAINT chk_prompt_audit_events_action CHECK (((action)::text = ANY ((ARRAY['Allow'::character varying, 'Warn'::character varying, 'Block'::character varying])::text[]))),
    CONSTRAINT chk_prompt_audit_events_categories_json CHECK ((jsonb_typeof(categories) = 'array'::text)),
    CONSTRAINT chk_prompt_audit_events_decision CHECK (((decision)::text = ANY ((ARRAY['pass'::character varying, 'flag'::character varying, 'critical'::character varying])::text[]))),
    CONSTRAINT chk_prompt_audit_events_evidence_json CHECK ((jsonb_typeof(scanner_evidence) = 'object'::text)),
    CONSTRAINT chk_prompt_audit_events_nonnegative CHECK (((policy_version >= 0) AND (config_version >= 1) AND (chunk_total >= 0) AND (latency_ms >= 0))),
    CONSTRAINT chk_prompt_audit_events_risk_level CHECK (((risk_level)::text = ANY ((ARRAY['low'::character varying, 'medium'::character varying, 'high'::character varying, 'critical'::character varying])::text[]))),
    CONSTRAINT chk_prompt_audit_events_scanners_json CHECK ((jsonb_typeof(matched_scanners) = 'array'::text)),
    CONSTRAINT chk_prompt_audit_events_scores_json CHECK ((jsonb_typeof(scanner_scores) = 'object'::text))
);


--
-- Name: prompt_audit_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.prompt_audit_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: prompt_audit_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.prompt_audit_events_id_seq OWNED BY public.prompt_audit_events.id;


--
-- Name: prompt_audit_jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.prompt_audit_jobs (
    id bigint NOT NULL,
    request_id character varying(128) DEFAULT ''::character varying NOT NULL,
    user_id bigint,
    username_snapshot character varying(255) DEFAULT ''::character varying NOT NULL,
    user_email_snapshot character varying(320) DEFAULT ''::character varying NOT NULL,
    api_key_id bigint,
    api_key_name_snapshot character varying(255) DEFAULT ''::character varying NOT NULL,
    group_id bigint,
    group_name character varying(255) DEFAULT ''::character varying NOT NULL,
    provider character varying(64) DEFAULT ''::character varying NOT NULL,
    endpoint character varying(128) DEFAULT ''::character varying NOT NULL,
    protocol character varying(64) DEFAULT ''::character varying NOT NULL,
    model character varying(255) DEFAULT ''::character varying NOT NULL,
    prompt_hash character varying(64) DEFAULT ''::character varying NOT NULL,
    redacted_preview text DEFAULT ''::text NOT NULL,
    prompt_length integer DEFAULT 0 NOT NULL,
    message_count integer DEFAULT 0 NOT NULL,
    stage character varying(32) DEFAULT 'http'::character varying NOT NULL,
    execution_mode character varying(32) DEFAULT 'async_audit'::character varying NOT NULL,
    config_version bigint DEFAULT 1 NOT NULL,
    status character varying(32) DEFAULT 'staging'::character varying NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    max_attempts integer DEFAULT 3 NOT NULL,
    claim_version bigint DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    processing_started_at timestamp with time zone,
    processed_at timestamp with time zone,
    last_error_code character varying(64) DEFAULT ''::character varying NOT NULL,
    last_error_message character varying(512) DEFAULT ''::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_prompt_audit_jobs_execution_mode CHECK (((execution_mode)::text = ANY ((ARRAY['async_audit'::character varying, 'blocking'::character varying])::text[]))),
    CONSTRAINT chk_prompt_audit_jobs_nonnegative CHECK (((attempts >= 0) AND (max_attempts >= 0) AND (claim_version >= 0) AND (prompt_length >= 0) AND (message_count >= 0) AND (config_version >= 1))),
    CONSTRAINT chk_prompt_audit_jobs_status CHECK (((status)::text = ANY ((ARRAY['staging'::character varying, 'queued'::character varying, 'processing'::character varying, 'retry'::character varying, 'done'::character varying, 'failed'::character varying])::text[])))
);


--
-- Name: prompt_audit_jobs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.prompt_audit_jobs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: prompt_audit_jobs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.prompt_audit_jobs_id_seq OWNED BY public.prompt_audit_jobs.id;


--
-- Name: proxies; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.proxies (
    id bigint NOT NULL,
    name character varying(100) NOT NULL,
    protocol character varying(20) NOT NULL,
    host character varying(255) NOT NULL,
    port integer NOT NULL,
    username character varying(100),
    password character varying(100),
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    expires_at timestamp with time zone,
    fallback_mode character varying(20) DEFAULT 'none'::character varying NOT NULL,
    backup_proxy_id bigint,
    expiry_warn_days integer DEFAULT 7 NOT NULL
);


--
-- Name: proxies_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.proxies_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: proxies_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.proxies_id_seq OWNED BY public.proxies.id;


--
-- Name: redeem_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.redeem_codes (
    id bigint NOT NULL,
    code character varying(32) NOT NULL,
    type character varying(20) DEFAULT 'balance'::character varying NOT NULL,
    value numeric(20,8) NOT NULL,
    status character varying(20) DEFAULT 'unused'::character varying NOT NULL,
    used_by bigint,
    used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    notes text,
    group_id bigint,
    validity_days integer DEFAULT 30 NOT NULL,
    expires_at timestamp with time zone
);


--
-- Name: COLUMN redeem_codes.notes; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.redeem_codes.notes IS '备注说明（管理员调整时的原因说明）';


--
-- Name: redeem_codes_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.redeem_codes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: redeem_codes_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.redeem_codes_id_seq OWNED BY public.redeem_codes.id;


--
-- Name: scheduled_test_plans; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.scheduled_test_plans (
    id bigint NOT NULL,
    account_id bigint NOT NULL,
    model_id character varying(100) DEFAULT ''::character varying NOT NULL,
    cron_expression character varying(100) DEFAULT '*/30 * * * *'::character varying NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    max_results integer DEFAULT 50 NOT NULL,
    last_run_at timestamp with time zone,
    next_run_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    auto_recover boolean DEFAULT false NOT NULL
);


--
-- Name: scheduled_test_plans_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.scheduled_test_plans_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: scheduled_test_plans_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.scheduled_test_plans_id_seq OWNED BY public.scheduled_test_plans.id;


--
-- Name: scheduled_test_results; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.scheduled_test_results (
    id bigint NOT NULL,
    plan_id bigint NOT NULL,
    status character varying(20) DEFAULT 'success'::character varying NOT NULL,
    response_text text DEFAULT ''::text NOT NULL,
    error_message text DEFAULT ''::text NOT NULL,
    latency_ms bigint DEFAULT 0 NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: scheduled_test_results_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.scheduled_test_results_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: scheduled_test_results_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.scheduled_test_results_id_seq OWNED BY public.scheduled_test_results.id;


--
-- Name: scheduler_outbox; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.scheduler_outbox (
    id bigint NOT NULL,
    event_type text NOT NULL,
    account_id bigint,
    group_id bigint,
    payload jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    dedup_key text
);


--
-- Name: scheduler_outbox_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.scheduler_outbox_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: scheduler_outbox_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.scheduler_outbox_id_seq OWNED BY public.scheduler_outbox.id;


--
-- Name: schema_migrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_migrations (
    filename text NOT NULL,
    checksum text NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: security_secrets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.security_secrets (
    id bigint NOT NULL,
    key character varying(100) NOT NULL,
    value text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: security_secrets_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.security_secrets_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: security_secrets_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.security_secrets_id_seq OWNED BY public.security_secrets.id;


--
-- Name: settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.settings (
    id bigint NOT NULL,
    key character varying(100) NOT NULL,
    value text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: settings_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.settings_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: settings_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.settings_id_seq OWNED BY public.settings.id;


--
-- Name: subscription_plans; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.subscription_plans (
    id bigint NOT NULL,
    group_id bigint NOT NULL,
    name character varying(100) NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    price numeric(20,2) NOT NULL,
    original_price numeric(20,2),
    validity_days integer DEFAULT 30 NOT NULL,
    validity_unit character varying(10) DEFAULT 'day'::character varying NOT NULL,
    features text DEFAULT ''::text NOT NULL,
    product_name character varying(100) DEFAULT ''::character varying NOT NULL,
    for_sale boolean DEFAULT true NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    currency character varying(3) DEFAULT ''::character varying NOT NULL
);


--
-- Name: subscription_plans_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.subscription_plans_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: subscription_plans_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.subscription_plans_id_seq OWNED BY public.subscription_plans.id;


--
-- Name: tls_fingerprint_profiles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tls_fingerprint_profiles (
    id bigint NOT NULL,
    name character varying(100) NOT NULL,
    description text,
    enable_grease boolean DEFAULT false NOT NULL,
    cipher_suites jsonb,
    curves jsonb,
    point_formats jsonb,
    signature_algorithms jsonb,
    alpn_protocols jsonb,
    supported_versions jsonb,
    key_share_groups jsonb,
    psk_modes jsonb,
    extensions jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE tls_fingerprint_profiles; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.tls_fingerprint_profiles IS 'TLS fingerprint templates for simulating specific client TLS handshake characteristics';


--
-- Name: COLUMN tls_fingerprint_profiles.name; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.tls_fingerprint_profiles.name IS 'Unique profile name, e.g. "macOS Node.js v24"';


--
-- Name: COLUMN tls_fingerprint_profiles.enable_grease; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.tls_fingerprint_profiles.enable_grease IS 'Whether to insert GREASE values in ClientHello extensions';


--
-- Name: COLUMN tls_fingerprint_profiles.cipher_suites; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.tls_fingerprint_profiles.cipher_suites IS 'TLS cipher suite list as JSON array of uint16 (order-sensitive, affects JA3)';


--
-- Name: COLUMN tls_fingerprint_profiles.extensions; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.tls_fingerprint_profiles.extensions IS 'TLS extension type IDs in send order as JSON array of uint16';


--
-- Name: tls_fingerprint_profiles_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.tls_fingerprint_profiles_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: tls_fingerprint_profiles_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.tls_fingerprint_profiles_id_seq OWNED BY public.tls_fingerprint_profiles.id;


--
-- Name: usage_billing_dedup; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_billing_dedup (
    id bigint NOT NULL,
    request_id character varying(255) NOT NULL,
    api_key_id bigint NOT NULL,
    request_fingerprint character varying(64) NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: usage_billing_dedup_archive; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_billing_dedup_archive (
    request_id character varying(255) NOT NULL,
    api_key_id bigint NOT NULL,
    request_fingerprint character varying(64) NOT NULL,
    created_at timestamp with time zone NOT NULL,
    archived_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: usage_billing_dedup_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.usage_billing_dedup_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: usage_billing_dedup_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.usage_billing_dedup_id_seq OWNED BY public.usage_billing_dedup.id;


--
-- Name: usage_cleanup_tasks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_cleanup_tasks (
    id bigint NOT NULL,
    status character varying(20) NOT NULL,
    filters jsonb NOT NULL,
    created_by bigint NOT NULL,
    deleted_rows bigint DEFAULT 0 NOT NULL,
    error_message text,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    canceled_by bigint,
    canceled_at timestamp with time zone
);


--
-- Name: usage_cleanup_tasks_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.usage_cleanup_tasks_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: usage_cleanup_tasks_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.usage_cleanup_tasks_id_seq OWNED BY public.usage_cleanup_tasks.id;


--
-- Name: usage_dashboard_aggregation_watermark; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_dashboard_aggregation_watermark (
    id integer NOT NULL,
    last_aggregated_at timestamp with time zone DEFAULT '1970-01-01 00:00:00+00'::timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: usage_dashboard_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_dashboard_daily (
    bucket_date date NOT NULL,
    total_requests bigint DEFAULT 0 NOT NULL,
    input_tokens bigint DEFAULT 0 NOT NULL,
    output_tokens bigint DEFAULT 0 NOT NULL,
    cache_creation_tokens bigint DEFAULT 0 NOT NULL,
    cache_read_tokens bigint DEFAULT 0 NOT NULL,
    total_cost numeric(20,10) DEFAULT 0 NOT NULL,
    actual_cost numeric(20,10) DEFAULT 0 NOT NULL,
    total_duration_ms bigint DEFAULT 0 NOT NULL,
    active_users bigint DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL,
    account_cost numeric(20,10) DEFAULT 0 NOT NULL
);


--
-- Name: TABLE usage_dashboard_daily; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.usage_dashboard_daily IS 'Pre-aggregated daily usage metrics for admin dashboard (UTC dates).';


--
-- Name: COLUMN usage_dashboard_daily.bucket_date; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_dashboard_daily.bucket_date IS 'UTC date of the day bucket.';


--
-- Name: COLUMN usage_dashboard_daily.computed_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_dashboard_daily.computed_at IS 'When the daily row was last computed/refreshed.';


--
-- Name: usage_dashboard_daily_users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_dashboard_daily_users (
    bucket_date date NOT NULL,
    user_id bigint NOT NULL
);


--
-- Name: usage_dashboard_hourly; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_dashboard_hourly (
    bucket_start timestamp with time zone NOT NULL,
    total_requests bigint DEFAULT 0 NOT NULL,
    input_tokens bigint DEFAULT 0 NOT NULL,
    output_tokens bigint DEFAULT 0 NOT NULL,
    cache_creation_tokens bigint DEFAULT 0 NOT NULL,
    cache_read_tokens bigint DEFAULT 0 NOT NULL,
    total_cost numeric(20,10) DEFAULT 0 NOT NULL,
    actual_cost numeric(20,10) DEFAULT 0 NOT NULL,
    total_duration_ms bigint DEFAULT 0 NOT NULL,
    active_users bigint DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL,
    account_cost numeric(20,10) DEFAULT 0 NOT NULL
);


--
-- Name: TABLE usage_dashboard_hourly; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.usage_dashboard_hourly IS 'Pre-aggregated hourly usage metrics for admin dashboard (UTC buckets).';


--
-- Name: COLUMN usage_dashboard_hourly.bucket_start; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_dashboard_hourly.bucket_start IS 'UTC start timestamp of the hour bucket.';


--
-- Name: COLUMN usage_dashboard_hourly.computed_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_dashboard_hourly.computed_at IS 'When the hourly row was last computed/refreshed.';


--
-- Name: usage_dashboard_hourly_users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_dashboard_hourly_users (
    bucket_start timestamp with time zone NOT NULL,
    user_id bigint NOT NULL
);


--
-- Name: usage_group_daily_rollups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_group_daily_rollups (
    bucket_date date NOT NULL,
    group_id bigint NOT NULL,
    actual_cost numeric(20,10) DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE usage_group_daily_rollups; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.usage_group_daily_rollups IS '按北京时间自然日聚合的分组实际费用。';


--
-- Name: COLUMN usage_group_daily_rollups.bucket_date; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_group_daily_rollups.bucket_date IS 'timezone_name 对应时区的自然日。';


--
-- Name: usage_group_rollup_state; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_group_rollup_state (
    id smallint NOT NULL,
    closed_before date DEFAULT '1970-01-01'::date NOT NULL,
    retained_from timestamp with time zone DEFAULT '1970-01-01 00:00:00+00'::timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    timezone_name text DEFAULT 'Asia/Shanghai'::text NOT NULL,
    CONSTRAINT usage_group_rollup_state_id_check CHECK ((id = 1))
);


--
-- Name: TABLE usage_group_rollup_state; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.usage_group_rollup_state IS '分组日汇总的单行发布水位。';


--
-- Name: COLUMN usage_group_rollup_state.closed_before; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_group_rollup_state.closed_before IS '已完整发布日桶的配置时区日期排他上界。';


--
-- Name: COLUMN usage_group_rollup_state.timezone_name; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_group_rollup_state.timezone_name IS '当前分组日桶采用的 IANA 时区名称。';


--
-- Name: usage_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_logs (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    api_key_id bigint NOT NULL,
    account_id bigint NOT NULL,
    request_id character varying(64),
    model character varying(100) NOT NULL,
    input_tokens integer DEFAULT 0 NOT NULL,
    output_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_tokens integer DEFAULT 0 NOT NULL,
    cache_read_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_5m_tokens integer DEFAULT 0 NOT NULL,
    cache_creation_1h_tokens integer DEFAULT 0 NOT NULL,
    input_cost numeric(20,10) DEFAULT 0 NOT NULL,
    output_cost numeric(20,10) DEFAULT 0 NOT NULL,
    cache_creation_cost numeric(20,10) DEFAULT 0 NOT NULL,
    cache_read_cost numeric(20,10) DEFAULT 0 NOT NULL,
    total_cost numeric(20,10) DEFAULT 0 NOT NULL,
    actual_cost numeric(20,10) DEFAULT 0 NOT NULL,
    stream boolean DEFAULT false NOT NULL,
    duration_ms integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    group_id bigint,
    subscription_id bigint,
    rate_multiplier numeric(10,4) DEFAULT 1 NOT NULL,
    first_token_ms integer,
    billing_type smallint DEFAULT 0 NOT NULL,
    user_agent character varying(512),
    image_count integer DEFAULT 0,
    image_size character varying(10),
    ip_address character varying(45),
    account_rate_multiplier numeric(10,4),
    reasoning_effort character varying(20),
    cache_ttl_overridden boolean DEFAULT false NOT NULL,
    openai_ws_mode boolean DEFAULT false NOT NULL,
    request_type smallint DEFAULT 0 NOT NULL,
    service_tier character varying(16),
    inbound_endpoint character varying(128),
    upstream_endpoint character varying(128),
    upstream_model character varying(100),
    requested_model character varying(100),
    channel_id bigint,
    model_mapping_chain character varying(500),
    billing_tier character varying(50),
    billing_mode character varying(20),
    image_output_tokens integer DEFAULT 0 NOT NULL,
    image_output_cost numeric(20,10) DEFAULT 0 NOT NULL,
    account_stats_cost numeric(20,10),
    image_input_size character varying(32),
    image_output_size character varying(32),
    image_size_source character varying(16),
    image_size_breakdown jsonb,
    video_count integer DEFAULT 0 NOT NULL,
    video_resolution character varying(10),
    video_duration_seconds integer,
    long_context_billing_applied boolean DEFAULT false NOT NULL,
    image_input_tokens integer DEFAULT 0 NOT NULL,
    image_input_cost numeric(20,10) DEFAULT 0 NOT NULL,
    session_id character varying(255),
    upstream_response_model character varying(200),
    upstream_model_mismatch boolean,
    native_compaction_v2 boolean DEFAULT false NOT NULL,
    requested_reasoning_effort character varying(20),
    upstream_request_id character varying(128),
    CONSTRAINT usage_logs_request_type_check CHECK (((request_type >= 0) AND (request_type <= 5)))
);


--
-- Name: COLUMN usage_logs.user_agent; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_logs.user_agent IS 'User-Agent header from the API request';


--
-- Name: COLUMN usage_logs.video_count; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_logs.video_count IS '视频生成数量；>0 表示本行是视频生成用量';


--
-- Name: COLUMN usage_logs.video_resolution; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_logs.video_resolution IS '计费用视频分辨率 480p/720p/1080p';


--
-- Name: COLUMN usage_logs.video_duration_seconds; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_logs.video_duration_seconds IS '提交时请求的视频时长（秒），按秒计费的乘数';


--
-- Name: COLUMN usage_logs.native_compaction_v2; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.usage_logs.native_compaction_v2 IS 'True only when the request was identified at runtime as native OpenAI remote compaction v2';


--
-- Name: usage_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.usage_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: usage_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.usage_logs_id_seq OWNED BY public.usage_logs.id;


--
-- Name: user_affiliate_ledger; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_affiliate_ledger (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    action character varying(32) NOT NULL,
    amount numeric(20,8) NOT NULL,
    source_user_id bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    frozen_until timestamp with time zone,
    source_order_id bigint,
    balance_after numeric(20,8),
    aff_quota_after numeric(20,8),
    aff_frozen_quota_after numeric(20,8),
    aff_history_quota_after numeric(20,8)
);


--
-- Name: TABLE user_affiliate_ledger; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.user_affiliate_ledger IS '邀请返利资金流水（累计/转入）';


--
-- Name: COLUMN user_affiliate_ledger.action; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliate_ledger.action IS 'accrue|transfer';


--
-- Name: COLUMN user_affiliate_ledger.frozen_until; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliate_ledger.frozen_until IS 'Rebate frozen until this time; NULL means already thawed or never frozen';


--
-- Name: COLUMN user_affiliate_ledger.source_order_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliate_ledger.source_order_id IS '产生该返利流水的充值订单；转余额或无法可靠回填的历史数据为 NULL';


--
-- Name: COLUMN user_affiliate_ledger.balance_after; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliate_ledger.balance_after IS '邀请返利转余额后的用户余额快照；无法取得时为 NULL';


--
-- Name: COLUMN user_affiliate_ledger.aff_quota_after; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliate_ledger.aff_quota_after IS '邀请返利转余额后的可用返利额度快照；无法取得时为 NULL';


--
-- Name: COLUMN user_affiliate_ledger.aff_frozen_quota_after; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliate_ledger.aff_frozen_quota_after IS '邀请返利转余额后的冻结返利额度快照；无法取得时为 NULL';


--
-- Name: COLUMN user_affiliate_ledger.aff_history_quota_after; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliate_ledger.aff_history_quota_after IS '邀请返利转余额后的历史返利总额快照；无法取得时为 NULL';


--
-- Name: user_affiliate_ledger_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_affiliate_ledger_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_affiliate_ledger_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_affiliate_ledger_id_seq OWNED BY public.user_affiliate_ledger.id;


--
-- Name: user_affiliates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_affiliates (
    user_id bigint NOT NULL,
    aff_code character varying(32) NOT NULL,
    inviter_id bigint,
    aff_count integer DEFAULT 0 NOT NULL,
    aff_quota numeric(20,8) DEFAULT 0 NOT NULL,
    aff_history_quota numeric(20,8) DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    aff_rebate_rate_percent numeric(5,2),
    aff_code_custom boolean DEFAULT false NOT NULL,
    aff_frozen_quota numeric(20,8) DEFAULT 0 NOT NULL
);


--
-- Name: TABLE user_affiliates; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.user_affiliates IS '用户邀请返利信息';


--
-- Name: COLUMN user_affiliates.aff_code; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliates.aff_code IS '用户邀请代码';


--
-- Name: COLUMN user_affiliates.inviter_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliates.inviter_id IS '邀请人用户ID';


--
-- Name: COLUMN user_affiliates.aff_count; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliates.aff_count IS '累计邀请人数';


--
-- Name: COLUMN user_affiliates.aff_quota; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliates.aff_quota IS '当前可提取返利金额';


--
-- Name: COLUMN user_affiliates.aff_history_quota; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliates.aff_history_quota IS '累计返利历史金额';


--
-- Name: COLUMN user_affiliates.aff_rebate_rate_percent; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliates.aff_rebate_rate_percent IS '专属返利比例（百分比 0-100，NULL 表示沿用全局）';


--
-- Name: COLUMN user_affiliates.aff_code_custom; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliates.aff_code_custom IS '邀请码是否由管理员改写过（用于专属用户筛选）';


--
-- Name: COLUMN user_affiliates.aff_frozen_quota; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_affiliates.aff_frozen_quota IS 'Rebate quota currently frozen (pending thaw after freeze period)';


--
-- Name: user_allowed_groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_allowed_groups (
    user_id bigint NOT NULL,
    group_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_attribute_definitions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_attribute_definitions (
    id bigint NOT NULL,
    key character varying(100) NOT NULL,
    name character varying(255) NOT NULL,
    description text DEFAULT ''::text,
    type character varying(20) NOT NULL,
    options jsonb DEFAULT '[]'::jsonb,
    required boolean DEFAULT false NOT NULL,
    validation jsonb DEFAULT '{}'::jsonb,
    placeholder character varying(255) DEFAULT ''::character varying,
    display_order integer DEFAULT 0 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);


--
-- Name: user_attribute_definitions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_attribute_definitions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_attribute_definitions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_attribute_definitions_id_seq OWNED BY public.user_attribute_definitions.id;


--
-- Name: user_attribute_values; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_attribute_values (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    attribute_id bigint NOT NULL,
    value text DEFAULT ''::text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_attribute_values_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_attribute_values_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_attribute_values_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_attribute_values_id_seq OWNED BY public.user_attribute_values.id;


--
-- Name: user_avatars; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_avatars (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    storage_provider character varying(20) DEFAULT 'database'::character varying NOT NULL,
    storage_key text DEFAULT ''::text NOT NULL,
    url text DEFAULT ''::text NOT NULL,
    content_type character varying(100) DEFAULT ''::character varying NOT NULL,
    byte_size integer DEFAULT 0 NOT NULL,
    sha256 character varying(64) DEFAULT ''::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_avatars_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_avatars_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_avatars_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_avatars_id_seq OWNED BY public.user_avatars.id;


--
-- Name: user_group_rate_multipliers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_group_rate_multipliers (
    user_id bigint NOT NULL,
    group_id bigint NOT NULL,
    rate_multiplier numeric(10,4),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    rpm_override integer
);


--
-- Name: TABLE user_group_rate_multipliers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.user_group_rate_multipliers IS '用户专属分组倍率配置';


--
-- Name: COLUMN user_group_rate_multipliers.user_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_group_rate_multipliers.user_id IS '用户ID';


--
-- Name: COLUMN user_group_rate_multipliers.group_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_group_rate_multipliers.group_id IS '分组ID';


--
-- Name: COLUMN user_group_rate_multipliers.rate_multiplier; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_group_rate_multipliers.rate_multiplier IS '专属计费倍率；NULL 表示沿用分组默认倍率。';


--
-- Name: COLUMN user_group_rate_multipliers.rpm_override; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_group_rate_multipliers.rpm_override IS '专属 RPM 上限；NULL 表示沿用分组默认；0 表示该用户在此分组不受 RPM 限制。';


--
-- Name: user_platform_quotas; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_platform_quotas (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    platform character varying(32) NOT NULL,
    daily_limit_usd numeric(20,10),
    weekly_limit_usd numeric(20,10),
    monthly_limit_usd numeric(20,10),
    daily_usage_usd numeric(20,10) DEFAULT 0 NOT NULL,
    weekly_usage_usd numeric(20,10) DEFAULT 0 NOT NULL,
    monthly_usage_usd numeric(20,10) DEFAULT 0 NOT NULL,
    daily_window_start timestamp with time zone,
    weekly_window_start timestamp with time zone,
    monthly_window_start timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT user_platform_quotas_platform_check CHECK (((platform)::text = ANY ((ARRAY['anthropic'::character varying, 'openai'::character varying, 'gemini'::character varying, 'antigravity'::character varying, 'grok'::character varying, 'kimi'::character varying, 'zhipu'::character varying, 'deepseek'::character varying, 'minimax'::character varying, 'opencode_go'::character varying])::text[])))
);


--
-- Name: user_platform_quotas_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_platform_quotas_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_platform_quotas_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_platform_quotas_id_seq OWNED BY public.user_platform_quotas.id;


--
-- Name: user_provider_default_grants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_provider_default_grants (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    provider_type character varying(20) NOT NULL,
    grant_reason character varying(20) DEFAULT 'first_bind'::character varying NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_provider_default_grants_provider_type_check CHECK (((provider_type)::text = ANY ((ARRAY['email'::character varying, 'linuxdo'::character varying, 'wechat'::character varying, 'oidc'::character varying, 'github'::character varying, 'google'::character varying, 'dingtalk'::character varying])::text[]))),
    CONSTRAINT user_provider_default_grants_reason_check CHECK (((grant_reason)::text = ANY ((ARRAY['signup'::character varying, 'first_bind'::character varying])::text[])))
);


--
-- Name: user_provider_default_grants_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_provider_default_grants_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_provider_default_grants_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_provider_default_grants_id_seq OWNED BY public.user_provider_default_grants.id;


--
-- Name: user_subscriptions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_subscriptions (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    group_id bigint NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    daily_window_start timestamp with time zone,
    weekly_window_start timestamp with time zone,
    monthly_window_start timestamp with time zone,
    daily_usage_usd numeric(20,10) DEFAULT 0 NOT NULL,
    weekly_usage_usd numeric(20,10) DEFAULT 0 NOT NULL,
    monthly_usage_usd numeric(20,10) DEFAULT 0 NOT NULL,
    assigned_by bigint,
    assigned_at timestamp with time zone DEFAULT now() NOT NULL,
    notes text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);


--
-- Name: COLUMN user_subscriptions.deleted_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_subscriptions.deleted_at IS '软删除时间戳，NULL 表示未删除';


--
-- Name: user_subscriptions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_subscriptions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_subscriptions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_subscriptions_id_seq OWNED BY public.user_subscriptions.id;


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id bigint NOT NULL,
    email character varying(255) NOT NULL,
    password_hash character varying(255) NOT NULL,
    role character varying(20) DEFAULT 'user'::character varying NOT NULL,
    balance numeric(20,8) DEFAULT 0 NOT NULL,
    concurrency integer DEFAULT 5 NOT NULL,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    username character varying(100) DEFAULT ''::character varying NOT NULL,
    notes text DEFAULT ''::text NOT NULL,
    wechat character varying(100) DEFAULT ''::character varying,
    totp_secret_encrypted text,
    totp_enabled boolean DEFAULT false NOT NULL,
    totp_enabled_at timestamp with time zone,
    balance_notify_enabled boolean DEFAULT true NOT NULL,
    balance_notify_threshold numeric(20,8) DEFAULT NULL::numeric,
    balance_notify_extra_emails text DEFAULT '[]'::text NOT NULL,
    balance_notify_threshold_type character varying(10) DEFAULT 'fixed'::character varying NOT NULL,
    total_recharged numeric(20,8) DEFAULT 0 NOT NULL,
    signup_source character varying(20) DEFAULT 'email'::character varying NOT NULL,
    last_login_at timestamp with time zone,
    last_active_at timestamp with time zone,
    rpm_limit integer DEFAULT 0 NOT NULL,
    frozen_balance numeric(20,8) DEFAULT 0 NOT NULL,
    restrict_public_groups boolean DEFAULT false NOT NULL,
    CONSTRAINT users_signup_source_check CHECK (((signup_source)::text = ANY ((ARRAY['email'::character varying, 'linuxdo'::character varying, 'wechat'::character varying, 'oidc'::character varying, 'github'::character varying, 'google'::character varying, 'dingtalk'::character varying])::text[])))
);


--
-- Name: TABLE users; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.users IS '用户表。注：原 allowed_groups BIGINT[] 列已迁移至 user_allowed_groups 联接表';


--
-- Name: COLUMN users.totp_secret_encrypted; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.users.totp_secret_encrypted IS 'AES-256-GCM 加密的 TOTP 密钥';


--
-- Name: COLUMN users.totp_enabled; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.users.totp_enabled IS '是否启用 TOTP 双因素认证';


--
-- Name: COLUMN users.totp_enabled_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.users.totp_enabled_at IS 'TOTP 启用时间';


--
-- Name: COLUMN users.rpm_limit; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.users.rpm_limit IS '用户级 RPM 兜底上限；0 表示不限制；仅当分组未设置 rpm_limit 时生效。';


--
-- Name: users_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.users_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: users_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.users_id_seq OWNED BY public.users.id;


--
-- Name: accounts id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts ALTER COLUMN id SET DEFAULT nextval('public.accounts_id_seq'::regclass);


--
-- Name: announcement_reads id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_reads ALTER COLUMN id SET DEFAULT nextval('public.announcement_reads_id_seq'::regclass);


--
-- Name: announcements id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcements ALTER COLUMN id SET DEFAULT nextval('public.announcements_id_seq'::regclass);


--
-- Name: api_keys id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys ALTER COLUMN id SET DEFAULT nextval('public.api_keys_id_seq'::regclass);


--
-- Name: audit_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_logs ALTER COLUMN id SET DEFAULT nextval('public.audit_logs_id_seq'::regclass);


--
-- Name: auth_cache_invalidation_outbox id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_cache_invalidation_outbox ALTER COLUMN id SET DEFAULT nextval('public.auth_cache_invalidation_outbox_id_seq'::regclass);


--
-- Name: auth_identities id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_identities ALTER COLUMN id SET DEFAULT nextval('public.auth_identities_id_seq'::regclass);


--
-- Name: auth_identity_channels id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_identity_channels ALTER COLUMN id SET DEFAULT nextval('public.auth_identity_channels_id_seq'::regclass);


--
-- Name: auth_identity_migration_reports id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_identity_migration_reports ALTER COLUMN id SET DEFAULT nextval('public.auth_identity_migration_reports_id_seq'::regclass);


--
-- Name: batch_image_events id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_events ALTER COLUMN id SET DEFAULT nextval('public.batch_image_events_id_seq'::regclass);


--
-- Name: batch_image_items id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_items ALTER COLUMN id SET DEFAULT nextval('public.batch_image_items_id_seq'::regclass);


--
-- Name: batch_image_jobs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_jobs ALTER COLUMN id SET DEFAULT nextval('public.batch_image_jobs_id_seq'::regclass);


--
-- Name: billing_usage_entries id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_usage_entries ALTER COLUMN id SET DEFAULT nextval('public.billing_usage_entries_id_seq'::regclass);


--
-- Name: channel_account_stats_model_pricing id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_model_pricing ALTER COLUMN id SET DEFAULT nextval('public.channel_account_stats_model_pricing_id_seq'::regclass);


--
-- Name: channel_account_stats_pricing_intervals id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_pricing_intervals ALTER COLUMN id SET DEFAULT nextval('public.channel_account_stats_pricing_intervals_id_seq'::regclass);


--
-- Name: channel_account_stats_pricing_rules id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_pricing_rules ALTER COLUMN id SET DEFAULT nextval('public.channel_account_stats_pricing_rules_id_seq'::regclass);


--
-- Name: channel_groups id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_groups ALTER COLUMN id SET DEFAULT nextval('public.channel_groups_id_seq'::regclass);


--
-- Name: channel_model_pricing id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_model_pricing ALTER COLUMN id SET DEFAULT nextval('public.channel_model_pricing_id_seq'::regclass);


--
-- Name: channel_monitor_daily_rollups id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_daily_rollups ALTER COLUMN id SET DEFAULT nextval('public.channel_monitor_daily_rollups_id_seq'::regclass);


--
-- Name: channel_monitor_histories id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_histories ALTER COLUMN id SET DEFAULT nextval('public.channel_monitor_histories_id_seq'::regclass);


--
-- Name: channel_monitor_request_templates id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_request_templates ALTER COLUMN id SET DEFAULT nextval('public.channel_monitor_request_templates_id_seq'::regclass);


--
-- Name: channel_monitors id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitors ALTER COLUMN id SET DEFAULT nextval('public.channel_monitors_id_seq'::regclass);


--
-- Name: channel_pricing_intervals id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_pricing_intervals ALTER COLUMN id SET DEFAULT nextval('public.channel_pricing_intervals_id_seq'::regclass);


--
-- Name: channels id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channels ALTER COLUMN id SET DEFAULT nextval('public.channels_id_seq'::regclass);


--
-- Name: composite_model_routes id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.composite_model_routes ALTER COLUMN id SET DEFAULT nextval('public.composite_model_routes_id_seq'::regclass);


--
-- Name: content_moderation_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.content_moderation_logs ALTER COLUMN id SET DEFAULT nextval('public.content_moderation_logs_id_seq'::regclass);


--
-- Name: deleted_api_key_audits id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deleted_api_key_audits ALTER COLUMN id SET DEFAULT nextval('public.deleted_api_key_audits_id_seq'::regclass);


--
-- Name: error_passthrough_rules id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.error_passthrough_rules ALTER COLUMN id SET DEFAULT nextval('public.error_passthrough_rules_id_seq'::regclass);


--
-- Name: groups id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.groups ALTER COLUMN id SET DEFAULT nextval('public.groups_id_seq'::regclass);


--
-- Name: idempotency_records id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.idempotency_records ALTER COLUMN id SET DEFAULT nextval('public.idempotency_records_id_seq'::regclass);


--
-- Name: identity_adoption_decisions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.identity_adoption_decisions ALTER COLUMN id SET DEFAULT nextval('public.identity_adoption_decisions_id_seq'::regclass);


--
-- Name: ops_alert_events id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_alert_events ALTER COLUMN id SET DEFAULT nextval('public.ops_alert_events_id_seq'::regclass);


--
-- Name: ops_alert_rules id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_alert_rules ALTER COLUMN id SET DEFAULT nextval('public.ops_alert_rules_id_seq'::regclass);


--
-- Name: ops_error_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_error_logs ALTER COLUMN id SET DEFAULT nextval('public.ops_error_logs_id_seq'::regclass);


--
-- Name: ops_ingress_reject_aggregates id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_ingress_reject_aggregates ALTER COLUMN id SET DEFAULT nextval('public.ops_ingress_reject_aggregates_id_seq'::regclass);


--
-- Name: ops_metrics_daily id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_metrics_daily ALTER COLUMN id SET DEFAULT nextval('public.ops_metrics_daily_id_seq'::regclass);


--
-- Name: ops_metrics_hourly id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_metrics_hourly ALTER COLUMN id SET DEFAULT nextval('public.ops_metrics_hourly_id_seq'::regclass);


--
-- Name: ops_system_log_cleanup_audits id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_system_log_cleanup_audits ALTER COLUMN id SET DEFAULT nextval('public.ops_system_log_cleanup_audits_id_seq'::regclass);


--
-- Name: ops_system_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_system_logs ALTER COLUMN id SET DEFAULT nextval('public.ops_system_logs_id_seq'::regclass);


--
-- Name: ops_system_metrics id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_system_metrics ALTER COLUMN id SET DEFAULT nextval('public.ops_system_metrics_id_seq'::regclass);


--
-- Name: orphan_allowed_groups_audit id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orphan_allowed_groups_audit ALTER COLUMN id SET DEFAULT nextval('public.orphan_allowed_groups_audit_id_seq'::regclass);


--
-- Name: passkey_credentials id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.passkey_credentials ALTER COLUMN id SET DEFAULT nextval('public.passkey_credentials_id_seq'::regclass);


--
-- Name: payment_audit_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payment_audit_logs ALTER COLUMN id SET DEFAULT nextval('public.payment_audit_logs_id_seq'::regclass);


--
-- Name: payment_orders id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payment_orders ALTER COLUMN id SET DEFAULT nextval('public.payment_orders_id_seq'::regclass);


--
-- Name: payment_provider_instances id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payment_provider_instances ALTER COLUMN id SET DEFAULT nextval('public.payment_provider_instances_id_seq'::regclass);


--
-- Name: pending_auth_sessions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_auth_sessions ALTER COLUMN id SET DEFAULT nextval('public.pending_auth_sessions_id_seq'::regclass);


--
-- Name: promo_code_usages id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.promo_code_usages ALTER COLUMN id SET DEFAULT nextval('public.promo_code_usages_id_seq'::regclass);


--
-- Name: promo_codes id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.promo_codes ALTER COLUMN id SET DEFAULT nextval('public.promo_codes_id_seq'::regclass);


--
-- Name: prompt_audit_events id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_events ALTER COLUMN id SET DEFAULT nextval('public.prompt_audit_events_id_seq'::regclass);


--
-- Name: prompt_audit_jobs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_jobs ALTER COLUMN id SET DEFAULT nextval('public.prompt_audit_jobs_id_seq'::regclass);


--
-- Name: proxies id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.proxies ALTER COLUMN id SET DEFAULT nextval('public.proxies_id_seq'::regclass);


--
-- Name: redeem_codes id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.redeem_codes ALTER COLUMN id SET DEFAULT nextval('public.redeem_codes_id_seq'::regclass);


--
-- Name: scheduled_test_plans id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduled_test_plans ALTER COLUMN id SET DEFAULT nextval('public.scheduled_test_plans_id_seq'::regclass);


--
-- Name: scheduled_test_results id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduled_test_results ALTER COLUMN id SET DEFAULT nextval('public.scheduled_test_results_id_seq'::regclass);


--
-- Name: scheduler_outbox id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduler_outbox ALTER COLUMN id SET DEFAULT nextval('public.scheduler_outbox_id_seq'::regclass);


--
-- Name: security_secrets id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.security_secrets ALTER COLUMN id SET DEFAULT nextval('public.security_secrets_id_seq'::regclass);


--
-- Name: settings id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.settings ALTER COLUMN id SET DEFAULT nextval('public.settings_id_seq'::regclass);


--
-- Name: subscription_plans id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscription_plans ALTER COLUMN id SET DEFAULT nextval('public.subscription_plans_id_seq'::regclass);


--
-- Name: tls_fingerprint_profiles id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_fingerprint_profiles ALTER COLUMN id SET DEFAULT nextval('public.tls_fingerprint_profiles_id_seq'::regclass);


--
-- Name: usage_billing_dedup id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_billing_dedup ALTER COLUMN id SET DEFAULT nextval('public.usage_billing_dedup_id_seq'::regclass);


--
-- Name: usage_cleanup_tasks id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_cleanup_tasks ALTER COLUMN id SET DEFAULT nextval('public.usage_cleanup_tasks_id_seq'::regclass);


--
-- Name: usage_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_logs ALTER COLUMN id SET DEFAULT nextval('public.usage_logs_id_seq'::regclass);


--
-- Name: user_affiliate_ledger id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliate_ledger ALTER COLUMN id SET DEFAULT nextval('public.user_affiliate_ledger_id_seq'::regclass);


--
-- Name: user_attribute_definitions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_attribute_definitions ALTER COLUMN id SET DEFAULT nextval('public.user_attribute_definitions_id_seq'::regclass);


--
-- Name: user_attribute_values id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_attribute_values ALTER COLUMN id SET DEFAULT nextval('public.user_attribute_values_id_seq'::regclass);


--
-- Name: user_avatars id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_avatars ALTER COLUMN id SET DEFAULT nextval('public.user_avatars_id_seq'::regclass);


--
-- Name: user_platform_quotas id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_platform_quotas ALTER COLUMN id SET DEFAULT nextval('public.user_platform_quotas_id_seq'::regclass);


--
-- Name: user_provider_default_grants id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_provider_default_grants ALTER COLUMN id SET DEFAULT nextval('public.user_provider_default_grants_id_seq'::regclass);


--
-- Name: user_subscriptions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_subscriptions ALTER COLUMN id SET DEFAULT nextval('public.user_subscriptions_id_seq'::regclass);


--
-- Name: users id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users ALTER COLUMN id SET DEFAULT nextval('public.users_id_seq'::regclass);


--
-- Name: account_groups account_groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_groups
    ADD CONSTRAINT account_groups_pkey PRIMARY KEY (account_id, group_id);


--
-- Name: accounts accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_pkey PRIMARY KEY (id);


--
-- Name: announcement_reads announcement_reads_announcement_id_user_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_reads
    ADD CONSTRAINT announcement_reads_announcement_id_user_id_key UNIQUE (announcement_id, user_id);


--
-- Name: announcement_reads announcement_reads_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_reads
    ADD CONSTRAINT announcement_reads_pkey PRIMARY KEY (id);


--
-- Name: announcements announcements_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcements
    ADD CONSTRAINT announcements_pkey PRIMARY KEY (id);


--
-- Name: api_keys api_keys_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_key_key UNIQUE (key);


--
-- Name: api_keys api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);


--
-- Name: audit_logs audit_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_logs
    ADD CONSTRAINT audit_logs_pkey PRIMARY KEY (id);


--
-- Name: auth_cache_invalidation_outbox auth_cache_invalidation_outbox_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_cache_invalidation_outbox
    ADD CONSTRAINT auth_cache_invalidation_outbox_pkey PRIMARY KEY (id);


--
-- Name: auth_identities auth_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_identities
    ADD CONSTRAINT auth_identities_pkey PRIMARY KEY (id);


--
-- Name: auth_identity_channels auth_identity_channels_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_identity_channels
    ADD CONSTRAINT auth_identity_channels_pkey PRIMARY KEY (id);


--
-- Name: auth_identity_migration_reports auth_identity_migration_reports_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_identity_migration_reports
    ADD CONSTRAINT auth_identity_migration_reports_pkey PRIMARY KEY (id);


--
-- Name: batch_image_events batch_image_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_events
    ADD CONSTRAINT batch_image_events_pkey PRIMARY KEY (id);


--
-- Name: batch_image_items batch_image_items_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_items
    ADD CONSTRAINT batch_image_items_pkey PRIMARY KEY (id);


--
-- Name: batch_image_jobs batch_image_jobs_batch_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_jobs
    ADD CONSTRAINT batch_image_jobs_batch_id_key UNIQUE (batch_id);


--
-- Name: batch_image_jobs batch_image_jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_jobs
    ADD CONSTRAINT batch_image_jobs_pkey PRIMARY KEY (id);


--
-- Name: billing_usage_entries billing_usage_entries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_usage_entries
    ADD CONSTRAINT billing_usage_entries_pkey PRIMARY KEY (id);


--
-- Name: channel_account_stats_model_pricing channel_account_stats_model_pricing_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_model_pricing
    ADD CONSTRAINT channel_account_stats_model_pricing_pkey PRIMARY KEY (id);


--
-- Name: channel_account_stats_pricing_intervals channel_account_stats_pricing_intervals_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_pricing_intervals
    ADD CONSTRAINT channel_account_stats_pricing_intervals_pkey PRIMARY KEY (id);


--
-- Name: channel_account_stats_pricing_rules channel_account_stats_pricing_rules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_pricing_rules
    ADD CONSTRAINT channel_account_stats_pricing_rules_pkey PRIMARY KEY (id);


--
-- Name: channel_groups channel_groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_groups
    ADD CONSTRAINT channel_groups_pkey PRIMARY KEY (id);


--
-- Name: channel_model_pricing channel_model_pricing_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_model_pricing
    ADD CONSTRAINT channel_model_pricing_pkey PRIMARY KEY (id);


--
-- Name: channel_monitor_aggregation_watermark channel_monitor_aggregation_watermark_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_aggregation_watermark
    ADD CONSTRAINT channel_monitor_aggregation_watermark_pkey PRIMARY KEY (id);


--
-- Name: channel_monitor_daily_rollups channel_monitor_daily_rollups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_daily_rollups
    ADD CONSTRAINT channel_monitor_daily_rollups_pkey PRIMARY KEY (id);


--
-- Name: channel_monitor_histories channel_monitor_histories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_histories
    ADD CONSTRAINT channel_monitor_histories_pkey PRIMARY KEY (id);


--
-- Name: channel_monitor_request_templates channel_monitor_request_templates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_request_templates
    ADD CONSTRAINT channel_monitor_request_templates_pkey PRIMARY KEY (id);


--
-- Name: channel_monitor_v2_config channel_monitor_v2_config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_config
    ADD CONSTRAINT channel_monitor_v2_config_pkey PRIMARY KEY (id);


--
-- Name: channel_monitor_v2_error_metrics_1m channel_monitor_v2_error_metrics_1m_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_error_metrics_1m
    ADD CONSTRAINT channel_monitor_v2_error_metrics_1m_pkey PRIMARY KEY (bucket_start, platform, group_id, model, error_category, taxonomy_version);


--
-- Name: channel_monitor_v2_error_metrics_rollup channel_monitor_v2_error_metrics_rollup_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_error_metrics_rollup
    ADD CONSTRAINT channel_monitor_v2_error_metrics_rollup_pkey PRIMARY KEY (bucket_seconds, bucket_start, platform, group_id, model, error_category, taxonomy_version);


--
-- Name: channel_monitor_v2_latency_histograms_1m channel_monitor_v2_latency_histograms_1m_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_latency_histograms_1m
    ADD CONSTRAINT channel_monitor_v2_latency_histograms_1m_pkey PRIMARY KEY (bucket_start, platform, group_id, model, user_id, metric, upper_bound_ms);


--
-- Name: channel_monitor_v2_latency_histograms_rollup channel_monitor_v2_latency_histograms_rollup_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_latency_histograms_rollup
    ADD CONSTRAINT channel_monitor_v2_latency_histograms_rollup_pkey PRIMARY KEY (bucket_seconds, bucket_start, platform, group_id, model, user_id, metric, upper_bound_ms);


--
-- Name: channel_monitor_v2_metrics_1m channel_monitor_v2_metrics_1m_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_metrics_1m
    ADD CONSTRAINT channel_monitor_v2_metrics_1m_pkey PRIMARY KEY (bucket_start, platform, group_id, model);


--
-- Name: channel_monitor_v2_metrics_rollup channel_monitor_v2_metrics_rollup_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_metrics_rollup
    ADD CONSTRAINT channel_monitor_v2_metrics_rollup_pkey PRIMARY KEY (bucket_seconds, bucket_start, platform, group_id, model);


--
-- Name: channel_monitor_v2_user_metrics_1m channel_monitor_v2_user_metrics_1m_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_user_metrics_1m
    ADD CONSTRAINT channel_monitor_v2_user_metrics_1m_pkey PRIMARY KEY (bucket_start, platform, group_id, model, user_id);


--
-- Name: channel_monitor_v2_user_metrics_rollup channel_monitor_v2_user_metrics_rollup_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_user_metrics_rollup
    ADD CONSTRAINT channel_monitor_v2_user_metrics_rollup_pkey PRIMARY KEY (bucket_seconds, bucket_start, platform, group_id, model, user_id);


--
-- Name: channel_monitor_v2_watermarks channel_monitor_v2_watermarks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_v2_watermarks
    ADD CONSTRAINT channel_monitor_v2_watermarks_pkey PRIMARY KEY (id);


--
-- Name: channel_monitors channel_monitors_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitors
    ADD CONSTRAINT channel_monitors_pkey PRIMARY KEY (id);


--
-- Name: channel_pricing_intervals channel_pricing_intervals_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_pricing_intervals
    ADD CONSTRAINT channel_pricing_intervals_pkey PRIMARY KEY (id);


--
-- Name: channels channels_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channels
    ADD CONSTRAINT channels_pkey PRIMARY KEY (id);


--
-- Name: composite_model_routes composite_model_routes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.composite_model_routes
    ADD CONSTRAINT composite_model_routes_pkey PRIMARY KEY (id);


--
-- Name: content_moderation_logs content_moderation_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.content_moderation_logs
    ADD CONSTRAINT content_moderation_logs_pkey PRIMARY KEY (id);


--
-- Name: deleted_api_key_audits deleted_api_key_audits_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deleted_api_key_audits
    ADD CONSTRAINT deleted_api_key_audits_pkey PRIMARY KEY (id);


--
-- Name: error_passthrough_rules error_passthrough_rules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.error_passthrough_rules
    ADD CONSTRAINT error_passthrough_rules_pkey PRIMARY KEY (id);


--
-- Name: groups groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.groups
    ADD CONSTRAINT groups_pkey PRIMARY KEY (id);


--
-- Name: idempotency_records idempotency_records_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.idempotency_records
    ADD CONSTRAINT idempotency_records_pkey PRIMARY KEY (id);


--
-- Name: identity_adoption_decisions identity_adoption_decisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.identity_adoption_decisions
    ADD CONSTRAINT identity_adoption_decisions_pkey PRIMARY KEY (id);


--
-- Name: ops_alert_events ops_alert_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_alert_events
    ADD CONSTRAINT ops_alert_events_pkey PRIMARY KEY (id);


--
-- Name: ops_alert_rules ops_alert_rules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_alert_rules
    ADD CONSTRAINT ops_alert_rules_pkey PRIMARY KEY (id);


--
-- Name: ops_error_logs ops_error_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_error_logs
    ADD CONSTRAINT ops_error_logs_pkey PRIMARY KEY (id);


--
-- Name: ops_ingress_reject_aggregates ops_ingress_reject_aggregates_dimensions_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_ingress_reject_aggregates
    ADD CONSTRAINT ops_ingress_reject_aggregates_dimensions_unique UNIQUE (bucket_start, reject_reason, route_family, protocol, client_ip, user_id, api_key_id);


--
-- Name: ops_ingress_reject_aggregates ops_ingress_reject_aggregates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_ingress_reject_aggregates
    ADD CONSTRAINT ops_ingress_reject_aggregates_pkey PRIMARY KEY (id);


--
-- Name: ops_job_heartbeats ops_job_heartbeats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_job_heartbeats
    ADD CONSTRAINT ops_job_heartbeats_pkey PRIMARY KEY (job_name);


--
-- Name: ops_metrics_daily ops_metrics_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_metrics_daily
    ADD CONSTRAINT ops_metrics_daily_pkey PRIMARY KEY (id);


--
-- Name: ops_metrics_hourly ops_metrics_hourly_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_metrics_hourly
    ADD CONSTRAINT ops_metrics_hourly_pkey PRIMARY KEY (id);


--
-- Name: ops_system_log_cleanup_audits ops_system_log_cleanup_audits_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_system_log_cleanup_audits
    ADD CONSTRAINT ops_system_log_cleanup_audits_pkey PRIMARY KEY (id);


--
-- Name: ops_system_logs ops_system_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_system_logs
    ADD CONSTRAINT ops_system_logs_pkey PRIMARY KEY (id);


--
-- Name: ops_system_metrics ops_system_metrics_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ops_system_metrics
    ADD CONSTRAINT ops_system_metrics_pkey PRIMARY KEY (id);


--
-- Name: orphan_allowed_groups_audit orphan_allowed_groups_audit_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orphan_allowed_groups_audit
    ADD CONSTRAINT orphan_allowed_groups_audit_pkey PRIMARY KEY (id);


--
-- Name: orphan_allowed_groups_audit orphan_allowed_groups_audit_user_id_group_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orphan_allowed_groups_audit
    ADD CONSTRAINT orphan_allowed_groups_audit_user_id_group_id_key UNIQUE (user_id, group_id);


--
-- Name: passkey_credentials passkey_credentials_credential_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.passkey_credentials
    ADD CONSTRAINT passkey_credentials_credential_id_key UNIQUE (credential_id);


--
-- Name: passkey_credentials passkey_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.passkey_credentials
    ADD CONSTRAINT passkey_credentials_pkey PRIMARY KEY (id);


--
-- Name: passkey_user_handles passkey_user_handles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.passkey_user_handles
    ADD CONSTRAINT passkey_user_handles_pkey PRIMARY KEY (user_id);


--
-- Name: passkey_user_handles passkey_user_handles_user_handle_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.passkey_user_handles
    ADD CONSTRAINT passkey_user_handles_user_handle_key UNIQUE (user_handle);


--
-- Name: payment_audit_logs payment_audit_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payment_audit_logs
    ADD CONSTRAINT payment_audit_logs_pkey PRIMARY KEY (id);


--
-- Name: payment_orders payment_orders_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payment_orders
    ADD CONSTRAINT payment_orders_pkey PRIMARY KEY (id);


--
-- Name: payment_provider_instances payment_provider_instances_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payment_provider_instances
    ADD CONSTRAINT payment_provider_instances_pkey PRIMARY KEY (id);


--
-- Name: pending_auth_sessions pending_auth_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_auth_sessions
    ADD CONSTRAINT pending_auth_sessions_pkey PRIMARY KEY (id);


--
-- Name: promo_code_usages promo_code_usages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.promo_code_usages
    ADD CONSTRAINT promo_code_usages_pkey PRIMARY KEY (id);


--
-- Name: promo_code_usages promo_code_usages_promo_code_id_user_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.promo_code_usages
    ADD CONSTRAINT promo_code_usages_promo_code_id_user_id_key UNIQUE (promo_code_id, user_id);


--
-- Name: promo_codes promo_codes_code_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.promo_codes
    ADD CONSTRAINT promo_codes_code_key UNIQUE (code);


--
-- Name: promo_codes promo_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.promo_codes
    ADD CONSTRAINT promo_codes_pkey PRIMARY KEY (id);


--
-- Name: prompt_audit_events prompt_audit_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_events
    ADD CONSTRAINT prompt_audit_events_pkey PRIMARY KEY (id);


--
-- Name: prompt_audit_jobs prompt_audit_jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_jobs
    ADD CONSTRAINT prompt_audit_jobs_pkey PRIMARY KEY (id);


--
-- Name: proxies proxies_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.proxies
    ADD CONSTRAINT proxies_pkey PRIMARY KEY (id);


--
-- Name: redeem_codes redeem_codes_code_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.redeem_codes
    ADD CONSTRAINT redeem_codes_code_key UNIQUE (code);


--
-- Name: redeem_codes redeem_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.redeem_codes
    ADD CONSTRAINT redeem_codes_pkey PRIMARY KEY (id);


--
-- Name: scheduled_test_plans scheduled_test_plans_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduled_test_plans
    ADD CONSTRAINT scheduled_test_plans_pkey PRIMARY KEY (id);


--
-- Name: scheduled_test_results scheduled_test_results_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduled_test_results
    ADD CONSTRAINT scheduled_test_results_pkey PRIMARY KEY (id);


--
-- Name: scheduler_outbox scheduler_outbox_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduler_outbox
    ADD CONSTRAINT scheduler_outbox_pkey PRIMARY KEY (id);


--
-- Name: schema_migrations schema_migrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (filename);


--
-- Name: security_secrets security_secrets_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.security_secrets
    ADD CONSTRAINT security_secrets_key_key UNIQUE (key);


--
-- Name: security_secrets security_secrets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.security_secrets
    ADD CONSTRAINT security_secrets_pkey PRIMARY KEY (id);


--
-- Name: settings settings_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.settings
    ADD CONSTRAINT settings_key_key UNIQUE (key);


--
-- Name: settings settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.settings
    ADD CONSTRAINT settings_pkey PRIMARY KEY (id);


--
-- Name: subscription_plans subscription_plans_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscription_plans
    ADD CONSTRAINT subscription_plans_pkey PRIMARY KEY (id);


--
-- Name: tls_fingerprint_profiles tls_fingerprint_profiles_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_fingerprint_profiles
    ADD CONSTRAINT tls_fingerprint_profiles_name_key UNIQUE (name);


--
-- Name: tls_fingerprint_profiles tls_fingerprint_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_fingerprint_profiles
    ADD CONSTRAINT tls_fingerprint_profiles_pkey PRIMARY KEY (id);


--
-- Name: usage_billing_dedup_archive usage_billing_dedup_archive_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_billing_dedup_archive
    ADD CONSTRAINT usage_billing_dedup_archive_pkey PRIMARY KEY (request_id, api_key_id);


--
-- Name: usage_billing_dedup usage_billing_dedup_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_billing_dedup
    ADD CONSTRAINT usage_billing_dedup_pkey PRIMARY KEY (id);


--
-- Name: usage_cleanup_tasks usage_cleanup_tasks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_cleanup_tasks
    ADD CONSTRAINT usage_cleanup_tasks_pkey PRIMARY KEY (id);


--
-- Name: usage_dashboard_aggregation_watermark usage_dashboard_aggregation_watermark_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_dashboard_aggregation_watermark
    ADD CONSTRAINT usage_dashboard_aggregation_watermark_pkey PRIMARY KEY (id);


--
-- Name: usage_dashboard_daily usage_dashboard_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_dashboard_daily
    ADD CONSTRAINT usage_dashboard_daily_pkey PRIMARY KEY (bucket_date);


--
-- Name: usage_dashboard_daily_users usage_dashboard_daily_users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_dashboard_daily_users
    ADD CONSTRAINT usage_dashboard_daily_users_pkey PRIMARY KEY (bucket_date, user_id);


--
-- Name: usage_dashboard_hourly usage_dashboard_hourly_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_dashboard_hourly
    ADD CONSTRAINT usage_dashboard_hourly_pkey PRIMARY KEY (bucket_start);


--
-- Name: usage_dashboard_hourly_users usage_dashboard_hourly_users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_dashboard_hourly_users
    ADD CONSTRAINT usage_dashboard_hourly_users_pkey PRIMARY KEY (bucket_start, user_id);


--
-- Name: usage_group_daily_rollups usage_group_daily_rollups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_group_daily_rollups
    ADD CONSTRAINT usage_group_daily_rollups_pkey PRIMARY KEY (bucket_date, group_id);


--
-- Name: usage_group_rollup_state usage_group_rollup_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_group_rollup_state
    ADD CONSTRAINT usage_group_rollup_state_pkey PRIMARY KEY (id);


--
-- Name: usage_logs usage_logs_image_billing_size_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.usage_logs
    ADD CONSTRAINT usage_logs_image_billing_size_check CHECK (((image_count <= 0) OR ((billing_mode)::text = 'video'::text) OR (COALESCE(video_count, 0) > 0) OR ((image_size IS NOT NULL) AND ((image_size)::text = ANY ((ARRAY['1K'::character varying, '2K'::character varying, '4K'::character varying, 'mixed'::character varying])::text[]))))) NOT VALID;


--
-- Name: usage_logs usage_logs_image_size_source_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.usage_logs
    ADD CONSTRAINT usage_logs_image_size_source_check CHECK (((image_size_source IS NULL) OR ((image_size_source)::text = ANY ((ARRAY['output'::character varying, 'input'::character varying, 'default'::character varying, 'legacy'::character varying])::text[])))) NOT VALID;


--
-- Name: usage_logs usage_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_logs
    ADD CONSTRAINT usage_logs_pkey PRIMARY KEY (id);


--
-- Name: user_affiliate_ledger user_affiliate_ledger_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliate_ledger
    ADD CONSTRAINT user_affiliate_ledger_pkey PRIMARY KEY (id);


--
-- Name: user_affiliates user_affiliates_aff_code_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliates
    ADD CONSTRAINT user_affiliates_aff_code_key UNIQUE (aff_code);


--
-- Name: user_affiliates user_affiliates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliates
    ADD CONSTRAINT user_affiliates_pkey PRIMARY KEY (user_id);


--
-- Name: user_allowed_groups user_allowed_groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_allowed_groups
    ADD CONSTRAINT user_allowed_groups_pkey PRIMARY KEY (user_id, group_id);


--
-- Name: user_attribute_definitions user_attribute_definitions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_attribute_definitions
    ADD CONSTRAINT user_attribute_definitions_pkey PRIMARY KEY (id);


--
-- Name: user_attribute_values user_attribute_values_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_attribute_values
    ADD CONSTRAINT user_attribute_values_pkey PRIMARY KEY (id);


--
-- Name: user_attribute_values user_attribute_values_user_id_attribute_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_attribute_values
    ADD CONSTRAINT user_attribute_values_user_id_attribute_id_key UNIQUE (user_id, attribute_id);


--
-- Name: user_avatars user_avatars_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_avatars
    ADD CONSTRAINT user_avatars_pkey PRIMARY KEY (id);


--
-- Name: user_group_rate_multipliers user_group_rate_multipliers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_group_rate_multipliers
    ADD CONSTRAINT user_group_rate_multipliers_pkey PRIMARY KEY (user_id, group_id);


--
-- Name: user_platform_quotas user_platform_quotas_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_pkey PRIMARY KEY (id);


--
-- Name: user_provider_default_grants user_provider_default_grants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_provider_default_grants
    ADD CONSTRAINT user_provider_default_grants_pkey PRIMARY KEY (id);


--
-- Name: user_subscriptions user_subscriptions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_subscriptions
    ADD CONSTRAINT user_subscriptions_pkey PRIMARY KEY (id);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: accounts_proxy_fallback_origin_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX accounts_proxy_fallback_origin_id_idx ON public.accounts USING btree (proxy_fallback_origin_id);


--
-- Name: auth_identities_provider_subject_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX auth_identities_provider_subject_key ON public.auth_identities USING btree (provider_type, provider_key, provider_subject);


--
-- Name: auth_identities_user_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX auth_identities_user_id_idx ON public.auth_identities USING btree (user_id);


--
-- Name: auth_identities_user_provider_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX auth_identities_user_provider_idx ON public.auth_identities USING btree (user_id, provider_type);


--
-- Name: auth_identity_channels_channel_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX auth_identity_channels_channel_key ON public.auth_identity_channels USING btree (provider_type, provider_key, channel, channel_app_id, channel_subject);


--
-- Name: auth_identity_channels_identity_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX auth_identity_channels_identity_id_idx ON public.auth_identity_channels USING btree (identity_id);


--
-- Name: auth_identity_migration_reports_type_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX auth_identity_migration_reports_type_idx ON public.auth_identity_migration_reports USING btree (report_type);


--
-- Name: auth_identity_migration_reports_type_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX auth_identity_migration_reports_type_key ON public.auth_identity_migration_reports USING btree (report_type, report_key);


--
-- Name: batch_image_events_event_type_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_events_event_type_idx ON public.batch_image_events USING btree (event_type);


--
-- Name: batch_image_events_job_created_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_events_job_created_at_idx ON public.batch_image_events USING btree (job_id, created_at);


--
-- Name: batch_image_events_job_event_hash_uq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX batch_image_events_job_event_hash_uq ON public.batch_image_events USING btree (job_id, event_hash) WHERE ((event_hash IS NOT NULL) AND ((event_hash)::text <> ''::text));


--
-- Name: batch_image_items_job_custom_uq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX batch_image_items_job_custom_uq ON public.batch_image_items USING btree (job_id, custom_id);


--
-- Name: batch_image_items_job_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_items_job_status_idx ON public.batch_image_items USING btree (job_id, status);


--
-- Name: batch_image_items_provider_source_object_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_items_provider_source_object_idx ON public.batch_image_items USING btree (provider_source_object);


--
-- Name: batch_image_jobs_downloaded_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_downloaded_at_idx ON public.batch_image_jobs USING btree (downloaded_at);


--
-- Name: batch_image_jobs_idempotency_key_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_idempotency_key_idx ON public.batch_image_jobs USING btree (idempotency_key) WHERE ((idempotency_key IS NOT NULL) AND ((idempotency_key)::text <> ''::text));


--
-- Name: batch_image_jobs_manifest_hash_uq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX batch_image_jobs_manifest_hash_uq ON public.batch_image_jobs USING btree (manifest_hash) WHERE ((manifest_hash IS NOT NULL) AND ((manifest_hash)::text <> ''::text));


--
-- Name: batch_image_jobs_output_expires_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_output_expires_at_idx ON public.batch_image_jobs USING btree (output_expires_at);


--
-- Name: batch_image_jobs_parent_batch_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_parent_batch_id_idx ON public.batch_image_jobs USING btree (parent_batch_id) WHERE ((parent_batch_id IS NOT NULL) AND ((parent_batch_id)::text <> ''::text));


--
-- Name: batch_image_jobs_provider_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_provider_status_idx ON public.batch_image_jobs USING btree (provider, status);


--
-- Name: batch_image_jobs_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_status_idx ON public.batch_image_jobs USING btree (status);


--
-- Name: batch_image_jobs_task_name_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_task_name_idx ON public.batch_image_jobs USING btree (task_name);


--
-- Name: batch_image_jobs_user_created_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_user_created_at_idx ON public.batch_image_jobs USING btree (user_id, created_at);


--
-- Name: batch_image_jobs_user_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX batch_image_jobs_user_deleted_at_idx ON public.batch_image_jobs USING btree (user_deleted_at);


--
-- Name: billing_usage_entries_usage_log_id_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX billing_usage_entries_usage_log_id_unique ON public.billing_usage_entries USING btree (usage_log_id);


--
-- Name: channel_monitor_request_templates_provider_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX channel_monitor_request_templates_provider_name ON public.channel_monitor_request_templates USING btree (provider, name);


--
-- Name: deletedapikeyaudit_key; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deletedapikeyaudit_key ON public.deleted_api_key_audits USING btree (key);


--
-- Name: deletedapikeyaudit_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deletedapikeyaudit_user_id ON public.deleted_api_key_audits USING btree (user_id);


--
-- Name: groups_name_unique_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX groups_name_unique_active ON public.groups USING btree (name) WHERE (deleted_at IS NULL);


--
-- Name: identity_adoption_decisions_identity_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX identity_adoption_decisions_identity_id_idx ON public.identity_adoption_decisions USING btree (identity_id);


--
-- Name: identity_adoption_decisions_pending_auth_session_id_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX identity_adoption_decisions_pending_auth_session_id_key ON public.identity_adoption_decisions USING btree (pending_auth_session_id);


--
-- Name: idx_account_groups_account_priority_group; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_account_groups_account_priority_group ON public.account_groups USING btree (account_id, priority, group_id);


--
-- Name: idx_account_groups_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_account_groups_group_id ON public.account_groups USING btree (group_id);


--
-- Name: idx_account_groups_group_priority_account; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_account_groups_group_priority_account ON public.account_groups USING btree (group_id, priority, account_id);


--
-- Name: idx_account_groups_priority; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_account_groups_priority ON public.account_groups USING btree (priority);


--
-- Name: idx_account_stats_pricing_intervals_pricing_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_account_stats_pricing_intervals_pricing_id ON public.channel_account_stats_pricing_intervals USING btree (pricing_id);


--
-- Name: idx_accounts_active_schedulable; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_active_schedulable ON public.accounts USING btree (priority, status) WHERE ((deleted_at IS NULL) AND (schedulable = true));


--
-- Name: idx_accounts_autopause_expiry_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_autopause_expiry_due ON public.accounts USING btree (expires_at) WHERE ((deleted_at IS NULL) AND (schedulable = true) AND (auto_pause_on_expired = true) AND (expires_at IS NOT NULL));


--
-- Name: idx_accounts_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_deleted_at ON public.accounts USING btree (deleted_at);


--
-- Name: idx_accounts_extra_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_extra_gin ON public.accounts USING gin (extra);


--
-- Name: idx_accounts_last_used_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_last_used_at ON public.accounts USING btree (last_used_at);


--
-- Name: idx_accounts_name_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_name_trgm ON public.accounts USING gin (name public.gin_trgm_ops);


--
-- Name: idx_accounts_overload_until; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_overload_until ON public.accounts USING btree (overload_until);


--
-- Name: idx_accounts_parent_account_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_parent_account_id ON public.accounts USING btree (parent_account_id) WHERE (parent_account_id IS NOT NULL);


--
-- Name: idx_accounts_platform; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_platform ON public.accounts USING btree (platform);


--
-- Name: idx_accounts_priority; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_priority ON public.accounts USING btree (priority);


--
-- Name: idx_accounts_proxy_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_proxy_id ON public.accounts USING btree (proxy_id);


--
-- Name: idx_accounts_rate_limit_reset_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_rate_limit_reset_at ON public.accounts USING btree (rate_limit_reset_at);


--
-- Name: idx_accounts_rate_limited_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_rate_limited_at ON public.accounts USING btree (rate_limited_at);


--
-- Name: idx_accounts_schedulable; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_schedulable ON public.accounts USING btree (schedulable);


--
-- Name: idx_accounts_schedulable_hot; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_schedulable_hot ON public.accounts USING btree (platform, priority) WHERE ((deleted_at IS NULL) AND ((status)::text = 'active'::text) AND (schedulable = true));


--
-- Name: idx_accounts_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_status ON public.accounts USING btree (status);


--
-- Name: idx_accounts_temp_unschedulable_until; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_temp_unschedulable_until ON public.accounts USING btree (temp_unschedulable_until) WHERE (deleted_at IS NULL);


--
-- Name: idx_accounts_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_type ON public.accounts USING btree (type);


--
-- Name: idx_announcement_reads_announcement_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_announcement_reads_announcement_id ON public.announcement_reads USING btree (announcement_id);


--
-- Name: idx_announcement_reads_read_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_announcement_reads_read_at ON public.announcement_reads USING btree (read_at);


--
-- Name: idx_announcement_reads_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_announcement_reads_user_id ON public.announcement_reads USING btree (user_id);


--
-- Name: idx_announcements_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_announcements_created_at ON public.announcements USING btree (created_at);


--
-- Name: idx_announcements_ends_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_announcements_ends_at ON public.announcements USING btree (ends_at);


--
-- Name: idx_announcements_starts_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_announcements_starts_at ON public.announcements USING btree (starts_at);


--
-- Name: idx_announcements_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_announcements_status ON public.announcements USING btree (status);


--
-- Name: idx_api_keys_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_deleted_at ON public.api_keys USING btree (deleted_at);


--
-- Name: idx_api_keys_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_expires_at ON public.api_keys USING btree (expires_at) WHERE (deleted_at IS NULL);


--
-- Name: idx_api_keys_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_group_id ON public.api_keys USING btree (group_id);


--
-- Name: idx_api_keys_key_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_key_trgm ON public.api_keys USING gin (key public.gin_trgm_ops);


--
-- Name: idx_api_keys_last_used_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_last_used_at ON public.api_keys USING btree (last_used_at) WHERE (deleted_at IS NULL);


--
-- Name: idx_api_keys_name_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_name_trgm ON public.api_keys USING gin (name public.gin_trgm_ops);


--
-- Name: idx_api_keys_quota_quota_used; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_quota_quota_used ON public.api_keys USING btree (quota, quota_used) WHERE (deleted_at IS NULL);


--
-- Name: idx_api_keys_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_status ON public.api_keys USING btree (status);


--
-- Name: idx_api_keys_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_user_id ON public.api_keys USING btree (user_id);


--
-- Name: idx_audit_logs_action; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_audit_logs_action ON public.audit_logs USING btree (action);


--
-- Name: idx_audit_logs_actor_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_audit_logs_actor_created ON public.audit_logs USING btree (actor_user_id, created_at DESC);


--
-- Name: idx_audit_logs_client_ip; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_audit_logs_client_ip ON public.audit_logs USING btree (client_ip);


--
-- Name: idx_audit_logs_created_at_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_audit_logs_created_at_id ON public.audit_logs USING btree (created_at DESC, id DESC);


--
-- Name: idx_auth_cache_invalidation_outbox_available; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_auth_cache_invalidation_outbox_available ON public.auth_cache_invalidation_outbox USING btree (available_at, id) WHERE (claimed_at IS NULL);


--
-- Name: idx_auth_cache_invalidation_outbox_cache_key; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_auth_cache_invalidation_outbox_cache_key ON public.auth_cache_invalidation_outbox USING btree (cache_key);


--
-- Name: idx_auth_cache_invalidation_outbox_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_auth_cache_invalidation_outbox_created_at ON public.auth_cache_invalidation_outbox USING btree (created_at);


--
-- Name: idx_auth_cache_invalidation_outbox_lease; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_auth_cache_invalidation_outbox_lease ON public.auth_cache_invalidation_outbox USING btree (claimed_at) WHERE (claimed_at IS NOT NULL);


--
-- Name: idx_auth_identity_migration_reports_resolved_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_auth_identity_migration_reports_resolved_at ON public.auth_identity_migration_reports USING btree (resolved_at);


--
-- Name: idx_billing_usage_entries_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_billing_usage_entries_created_at ON public.billing_usage_entries USING btree (created_at);


--
-- Name: idx_billing_usage_entries_user_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_billing_usage_entries_user_time ON public.billing_usage_entries USING btree (user_id, created_at);


--
-- Name: idx_cas_model_pricing_rule_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cas_model_pricing_rule_id ON public.channel_account_stats_model_pricing USING btree (rule_id);


--
-- Name: idx_cas_pricing_rules_channel_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cas_pricing_rules_channel_id ON public.channel_account_stats_pricing_rules USING btree (channel_id);


--
-- Name: idx_channel_groups_channel_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_groups_channel_id ON public.channel_groups USING btree (channel_id);


--
-- Name: idx_channel_groups_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_channel_groups_group_id ON public.channel_groups USING btree (group_id);


--
-- Name: idx_channel_model_pricing_channel_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_model_pricing_channel_id ON public.channel_model_pricing USING btree (channel_id);


--
-- Name: idx_channel_model_pricing_platform; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_model_pricing_platform ON public.channel_model_pricing USING btree (platform);


--
-- Name: idx_channel_monitor_daily_rollups_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_daily_rollups_bucket ON public.channel_monitor_daily_rollups USING btree (bucket_date);


--
-- Name: idx_channel_monitor_daily_rollups_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_channel_monitor_daily_rollups_unique ON public.channel_monitor_daily_rollups USING btree (monitor_id, model, bucket_date);


--
-- Name: idx_channel_monitor_histories_checked_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_histories_checked_at ON public.channel_monitor_histories USING btree (checked_at);


--
-- Name: idx_channel_monitor_histories_monitor_model_checked; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_histories_monitor_model_checked ON public.channel_monitor_histories USING btree (monitor_id, model, checked_at DESC);


--
-- Name: idx_channel_monitor_templates_provider_api_mode; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_templates_provider_api_mode ON public.channel_monitor_request_templates USING btree (provider, api_mode);


--
-- Name: idx_channel_monitor_v2_errors_category_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_errors_category_time ON public.channel_monitor_v2_error_metrics_1m USING btree (error_category, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_errors_rollup_category_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_errors_rollup_category_time ON public.channel_monitor_v2_error_metrics_rollup USING btree (bucket_seconds, error_category, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_errors_rollup_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_errors_rollup_time ON public.channel_monitor_v2_error_metrics_rollup USING btree (bucket_seconds, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_errors_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_errors_time ON public.channel_monitor_v2_error_metrics_1m USING btree (bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_histograms_rollup_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_histograms_rollup_time ON public.channel_monitor_v2_latency_histograms_rollup USING btree (bucket_seconds, bucket_start DESC, metric);


--
-- Name: idx_channel_monitor_v2_histograms_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_histograms_time ON public.channel_monitor_v2_latency_histograms_1m USING btree (bucket_start DESC, metric);


--
-- Name: idx_channel_monitor_v2_metrics_group_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_metrics_group_time ON public.channel_monitor_v2_metrics_1m USING btree (group_id, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_metrics_model_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_metrics_model_time ON public.channel_monitor_v2_metrics_1m USING btree (model, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_metrics_platform_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_metrics_platform_time ON public.channel_monitor_v2_metrics_1m USING btree (platform, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_metrics_rollup_group_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_metrics_rollup_group_time ON public.channel_monitor_v2_metrics_rollup USING btree (bucket_seconds, group_id, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_metrics_rollup_model_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_metrics_rollup_model_time ON public.channel_monitor_v2_metrics_rollup USING btree (bucket_seconds, model, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_metrics_rollup_platform_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_metrics_rollup_platform_time ON public.channel_monitor_v2_metrics_rollup USING btree (bucket_seconds, platform, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_user_metrics_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_user_metrics_time ON public.channel_monitor_v2_user_metrics_1m USING btree (bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_user_metrics_user_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_user_metrics_user_time ON public.channel_monitor_v2_user_metrics_1m USING btree (user_id, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_user_rollup_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_user_rollup_time ON public.channel_monitor_v2_user_metrics_rollup USING btree (bucket_seconds, bucket_start DESC);


--
-- Name: idx_channel_monitor_v2_user_rollup_user_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitor_v2_user_rollup_user_time ON public.channel_monitor_v2_user_metrics_rollup USING btree (bucket_seconds, user_id, bucket_start DESC);


--
-- Name: idx_channel_monitors_account_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitors_account_id ON public.channel_monitors USING btree (account_id);


--
-- Name: idx_channel_monitors_enabled_last_checked; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitors_enabled_last_checked ON public.channel_monitors USING btree (enabled, last_checked_at);


--
-- Name: idx_channel_monitors_group_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitors_group_name ON public.channel_monitors USING btree (group_name);


--
-- Name: idx_channel_monitors_provider; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitors_provider ON public.channel_monitors USING btree (provider);


--
-- Name: idx_channel_monitors_provider_api_mode; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitors_provider_api_mode ON public.channel_monitors USING btree (provider, api_mode);


--
-- Name: idx_channel_monitors_template_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_monitors_template_id ON public.channel_monitors USING btree (template_id) WHERE (template_id IS NOT NULL);


--
-- Name: idx_channel_pricing_intervals_pricing_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channel_pricing_intervals_pricing_id ON public.channel_pricing_intervals USING btree (pricing_id);


--
-- Name: idx_channels_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_channels_name ON public.channels USING btree (name);


--
-- Name: idx_channels_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_channels_status ON public.channels USING btree (status);


--
-- Name: idx_composite_model_routes_group_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_composite_model_routes_group_enabled ON public.composite_model_routes USING btree (group_id, enabled) WHERE (deleted_at IS NULL);


--
-- Name: idx_composite_model_routes_group_priority; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_composite_model_routes_group_priority ON public.composite_model_routes USING btree (group_id, priority, id) WHERE (deleted_at IS NULL);


--
-- Name: idx_composite_model_routes_unique_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_composite_model_routes_unique_active ON public.composite_model_routes USING btree (group_id, endpoint, match_type, public_model) WHERE (deleted_at IS NULL);


--
-- Name: idx_content_moderation_logs_api_key_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_content_moderation_logs_api_key_created_at ON public.content_moderation_logs USING btree (api_key_id, created_at DESC);


--
-- Name: idx_content_moderation_logs_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_content_moderation_logs_created_at ON public.content_moderation_logs USING btree (created_at DESC);


--
-- Name: idx_content_moderation_logs_endpoint_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_content_moderation_logs_endpoint_created_at ON public.content_moderation_logs USING btree (endpoint, created_at DESC);


--
-- Name: idx_content_moderation_logs_flagged_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_content_moderation_logs_flagged_created_at ON public.content_moderation_logs USING btree (flagged, created_at DESC);


--
-- Name: idx_content_moderation_logs_group_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_content_moderation_logs_group_created_at ON public.content_moderation_logs USING btree (group_id, created_at DESC);


--
-- Name: idx_content_moderation_logs_user_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_content_moderation_logs_user_created_at ON public.content_moderation_logs USING btree (user_id, created_at DESC);


--
-- Name: idx_error_passthrough_rules_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_error_passthrough_rules_enabled ON public.error_passthrough_rules USING btree (enabled);


--
-- Name: idx_error_passthrough_rules_priority; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_error_passthrough_rules_priority ON public.error_passthrough_rules USING btree (priority);


--
-- Name: idx_groups_claude_code_only; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_claude_code_only ON public.groups USING btree (claude_code_only) WHERE (deleted_at IS NULL);


--
-- Name: idx_groups_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_deleted_at ON public.groups USING btree (deleted_at);


--
-- Name: idx_groups_duplicate_operation_id_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_groups_duplicate_operation_id_active ON public.groups USING btree (duplicate_operation_id) WHERE ((duplicate_operation_id IS NOT NULL) AND (deleted_at IS NULL));


--
-- Name: idx_groups_fallback_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_fallback_group_id ON public.groups USING btree (fallback_group_id) WHERE ((deleted_at IS NULL) AND (fallback_group_id IS NOT NULL));


--
-- Name: idx_groups_fallback_group_id_on_invalid_request; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_fallback_group_id_on_invalid_request ON public.groups USING btree (fallback_group_id_on_invalid_request) WHERE ((deleted_at IS NULL) AND (fallback_group_id_on_invalid_request IS NOT NULL));


--
-- Name: idx_groups_is_exclusive; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_is_exclusive ON public.groups USING btree (is_exclusive);


--
-- Name: idx_groups_platform; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_platform ON public.groups USING btree (platform);


--
-- Name: idx_groups_sort_order; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_sort_order ON public.groups USING btree (sort_order);


--
-- Name: idx_groups_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_status ON public.groups USING btree (status);


--
-- Name: idx_groups_subscription_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_groups_subscription_type ON public.groups USING btree (subscription_type);


--
-- Name: idx_idempotency_records_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_idempotency_records_expires_at ON public.idempotency_records USING btree (expires_at);


--
-- Name: idx_idempotency_records_scope_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_idempotency_records_scope_key ON public.idempotency_records USING btree (scope, idempotency_key_hash);


--
-- Name: idx_idempotency_records_status_locked_until; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_idempotency_records_status_locked_until ON public.idempotency_records USING btree (status, locked_until);


--
-- Name: idx_ops_alert_events_fired_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_alert_events_fired_at ON public.ops_alert_events USING btree (fired_at DESC);


--
-- Name: idx_ops_alert_events_rule_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_alert_events_rule_status ON public.ops_alert_events USING btree (rule_id, status);


--
-- Name: idx_ops_alert_rules_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_alert_rules_enabled ON public.ops_alert_rules USING btree (enabled);


--
-- Name: idx_ops_alert_rules_name_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_ops_alert_rules_name_unique ON public.ops_alert_rules USING btree (name);


--
-- Name: idx_ops_error_logs_account_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_account_time ON public.ops_error_logs USING btree (account_id, created_at DESC) WHERE (account_id IS NOT NULL);


--
-- Name: idx_ops_error_logs_client_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_client_request_id ON public.ops_error_logs USING btree (client_request_id);


--
-- Name: idx_ops_error_logs_client_request_id_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_client_request_id_trgm ON public.ops_error_logs USING gin (client_request_id public.gin_trgm_ops);


--
-- Name: idx_ops_error_logs_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_created_at ON public.ops_error_logs USING btree (created_at DESC);


--
-- Name: idx_ops_error_logs_error_message_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_error_message_trgm ON public.ops_error_logs USING gin (error_message public.gin_trgm_ops);


--
-- Name: idx_ops_error_logs_group_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_group_time ON public.ops_error_logs USING btree (group_id, created_at DESC) WHERE (group_id IS NOT NULL);


--
-- Name: idx_ops_error_logs_is_count_tokens; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_is_count_tokens ON public.ops_error_logs USING btree (is_count_tokens) WHERE (is_count_tokens = true);


--
-- Name: idx_ops_error_logs_phase_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_phase_time ON public.ops_error_logs USING btree (error_phase, created_at DESC);


--
-- Name: idx_ops_error_logs_platform_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_platform_time ON public.ops_error_logs USING btree (platform, created_at DESC);


--
-- Name: idx_ops_error_logs_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_request_id ON public.ops_error_logs USING btree (request_id);


--
-- Name: idx_ops_error_logs_request_id_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_request_id_trgm ON public.ops_error_logs USING gin (request_id public.gin_trgm_ops);


--
-- Name: idx_ops_error_logs_resolved_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_resolved_time ON public.ops_error_logs USING btree (resolved, created_at DESC);


--
-- Name: idx_ops_error_logs_status_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_status_time ON public.ops_error_logs USING btree (status_code, created_at DESC);


--
-- Name: idx_ops_error_logs_type_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_type_time ON public.ops_error_logs USING btree (error_type, created_at DESC);


--
-- Name: idx_ops_error_logs_unresolved_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_unresolved_time ON public.ops_error_logs USING btree (created_at DESC) WHERE (resolved = false);


--
-- Name: idx_ops_error_logs_user_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_error_logs_user_time ON public.ops_error_logs USING btree (user_id, created_at DESC) WHERE (user_id IS NOT NULL);


--
-- Name: idx_ops_ingress_reject_aggregates_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_ingress_reject_aggregates_bucket ON public.ops_ingress_reject_aggregates USING btree (bucket_start DESC);


--
-- Name: idx_ops_ingress_reject_aggregates_ip_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_ingress_reject_aggregates_ip_bucket ON public.ops_ingress_reject_aggregates USING btree (client_ip, bucket_start DESC);


--
-- Name: idx_ops_ingress_reject_aggregates_reason_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_ingress_reject_aggregates_reason_bucket ON public.ops_ingress_reject_aggregates USING btree (reject_reason, bucket_start DESC);


--
-- Name: idx_ops_metrics_daily_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_metrics_daily_bucket ON public.ops_metrics_daily USING btree (bucket_date DESC);


--
-- Name: idx_ops_metrics_daily_group_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_metrics_daily_group_bucket ON public.ops_metrics_daily USING btree (group_id, bucket_date DESC) WHERE ((group_id IS NOT NULL) AND (group_id <> 0));


--
-- Name: idx_ops_metrics_daily_platform_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_metrics_daily_platform_bucket ON public.ops_metrics_daily USING btree (platform, bucket_date DESC) WHERE ((platform IS NOT NULL) AND ((platform)::text <> ''::text) AND (group_id IS NULL));


--
-- Name: idx_ops_metrics_daily_unique_dim; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_ops_metrics_daily_unique_dim ON public.ops_metrics_daily USING btree (bucket_date, COALESCE(platform, ''::character varying), COALESCE(group_id, (0)::bigint));


--
-- Name: idx_ops_metrics_hourly_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_metrics_hourly_bucket ON public.ops_metrics_hourly USING btree (bucket_start DESC);


--
-- Name: idx_ops_metrics_hourly_group_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_metrics_hourly_group_bucket ON public.ops_metrics_hourly USING btree (group_id, bucket_start DESC) WHERE ((group_id IS NOT NULL) AND (group_id <> 0));


--
-- Name: idx_ops_metrics_hourly_platform_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_metrics_hourly_platform_bucket ON public.ops_metrics_hourly USING btree (platform, bucket_start DESC) WHERE ((platform IS NOT NULL) AND ((platform)::text <> ''::text) AND (group_id IS NULL));


--
-- Name: idx_ops_metrics_hourly_unique_dim; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_ops_metrics_hourly_unique_dim ON public.ops_metrics_hourly USING btree (bucket_start, COALESCE(platform, ''::character varying), COALESCE(group_id, (0)::bigint));


--
-- Name: idx_ops_system_log_cleanup_audits_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_log_cleanup_audits_created_at ON public.ops_system_log_cleanup_audits USING btree (created_at DESC, id DESC);


--
-- Name: idx_ops_system_logs_account_id_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_account_id_created_at ON public.ops_system_logs USING btree (account_id, created_at DESC);


--
-- Name: idx_ops_system_logs_api_key_id_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_api_key_id_created_at ON public.ops_system_logs USING btree (api_key_id, created_at DESC);


--
-- Name: idx_ops_system_logs_client_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_client_request_id ON public.ops_system_logs USING btree (client_request_id);


--
-- Name: idx_ops_system_logs_component_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_component_created_at ON public.ops_system_logs USING btree (component, created_at DESC);


--
-- Name: idx_ops_system_logs_created_at_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_created_at_id ON public.ops_system_logs USING btree (created_at DESC, id DESC);


--
-- Name: idx_ops_system_logs_host_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_host_created_at ON public.ops_system_logs USING btree (host, created_at DESC);


--
-- Name: idx_ops_system_logs_level_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_level_created_at ON public.ops_system_logs USING btree (level, created_at DESC);


--
-- Name: idx_ops_system_logs_message_search; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_message_search ON public.ops_system_logs USING gin (to_tsvector('simple'::regconfig, COALESCE(message, ''::text)));


--
-- Name: idx_ops_system_logs_platform_model_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_platform_model_created_at ON public.ops_system_logs USING btree (platform, model, created_at DESC);


--
-- Name: idx_ops_system_logs_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_request_id ON public.ops_system_logs USING btree (request_id);


--
-- Name: idx_ops_system_logs_user_id_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_logs_user_id_created_at ON public.ops_system_logs USING btree (user_id, created_at DESC);


--
-- Name: idx_ops_system_metrics_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_metrics_created_at ON public.ops_system_metrics USING btree (created_at DESC);


--
-- Name: idx_ops_system_metrics_group_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_metrics_group_time ON public.ops_system_metrics USING btree (group_id, created_at DESC) WHERE (group_id IS NOT NULL);


--
-- Name: idx_ops_system_metrics_platform_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_metrics_platform_time ON public.ops_system_metrics USING btree (platform, created_at DESC) WHERE ((platform IS NOT NULL) AND ((platform)::text <> ''::text) AND (group_id IS NULL));


--
-- Name: idx_ops_system_metrics_window_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_system_metrics_window_time ON public.ops_system_metrics USING btree (window_minutes, created_at DESC);


--
-- Name: idx_orphan_allowed_groups_audit_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_orphan_allowed_groups_audit_user_id ON public.orphan_allowed_groups_audit USING btree (user_id);


--
-- Name: idx_payment_audit_logs_order_action_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_payment_audit_logs_order_action_uniq ON public.payment_audit_logs USING btree (order_id, action);


--
-- Name: idx_payment_audit_logs_order_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_audit_logs_order_id ON public.payment_audit_logs USING btree (order_id);


--
-- Name: idx_payment_orders_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_orders_created_at ON public.payment_orders USING btree (created_at);


--
-- Name: idx_payment_orders_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_orders_expires_at ON public.payment_orders USING btree (expires_at);


--
-- Name: idx_payment_orders_order_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_orders_order_type ON public.payment_orders USING btree (order_type);


--
-- Name: idx_payment_orders_paid_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_orders_paid_at ON public.payment_orders USING btree (paid_at);


--
-- Name: idx_payment_orders_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_orders_status ON public.payment_orders USING btree (status);


--
-- Name: idx_payment_orders_type_paid; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_orders_type_paid ON public.payment_orders USING btree (payment_type, paid_at);


--
-- Name: idx_payment_orders_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_orders_user_id ON public.payment_orders USING btree (user_id);


--
-- Name: idx_payment_provider_instances_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_provider_instances_enabled ON public.payment_provider_instances USING btree (enabled);


--
-- Name: idx_payment_provider_instances_provider_key; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_payment_provider_instances_provider_key ON public.payment_provider_instances USING btree (provider_key);


--
-- Name: idx_promo_code_usages_promo_code_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_promo_code_usages_promo_code_id ON public.promo_code_usages USING btree (promo_code_id);


--
-- Name: idx_promo_code_usages_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_promo_code_usages_user_id ON public.promo_code_usages USING btree (user_id);


--
-- Name: idx_promo_codes_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_promo_codes_expires_at ON public.promo_codes USING btree (expires_at);


--
-- Name: idx_promo_codes_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_promo_codes_status ON public.promo_codes USING btree (status);


--
-- Name: idx_prompt_audit_events_api_key_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_api_key_created ON public.prompt_audit_events USING btree (api_key_id, created_at DESC, id DESC);


--
-- Name: idx_prompt_audit_events_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_created ON public.prompt_audit_events USING btree (created_at DESC, id DESC);


--
-- Name: idx_prompt_audit_events_decision_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_decision_created ON public.prompt_audit_events USING btree (decision, created_at DESC, id DESC);


--
-- Name: idx_prompt_audit_events_group_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_group_created ON public.prompt_audit_events USING btree (group_id, created_at DESC, id DESC);


--
-- Name: idx_prompt_audit_events_job; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_job ON public.prompt_audit_events USING btree (job_id);


--
-- Name: idx_prompt_audit_events_prompt_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_prompt_hash ON public.prompt_audit_events USING btree (prompt_hash);


--
-- Name: idx_prompt_audit_events_request; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_request ON public.prompt_audit_events USING btree (request_id);


--
-- Name: idx_prompt_audit_events_risk_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_risk_created ON public.prompt_audit_events USING btree (risk_level, created_at DESC, id DESC);


--
-- Name: idx_prompt_audit_events_user_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_events_user_created ON public.prompt_audit_events USING btree (user_id, created_at DESC, id DESC);


--
-- Name: idx_prompt_audit_jobs_api_key_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_jobs_api_key_created ON public.prompt_audit_jobs USING btree (api_key_id, created_at DESC);


--
-- Name: idx_prompt_audit_jobs_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_jobs_created ON public.prompt_audit_jobs USING btree (created_at DESC, id DESC);


--
-- Name: idx_prompt_audit_jobs_group_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_jobs_group_created ON public.prompt_audit_jobs USING btree (group_id, created_at DESC);


--
-- Name: idx_prompt_audit_jobs_prompt_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_jobs_prompt_hash ON public.prompt_audit_jobs USING btree (prompt_hash);


--
-- Name: idx_prompt_audit_jobs_request; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_jobs_request ON public.prompt_audit_jobs USING btree (request_id);


--
-- Name: idx_prompt_audit_jobs_schedule; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_jobs_schedule ON public.prompt_audit_jobs USING btree (status, next_attempt_at, id);


--
-- Name: idx_prompt_audit_jobs_user_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_prompt_audit_jobs_user_created ON public.prompt_audit_jobs USING btree (user_id, created_at DESC);


--
-- Name: idx_proxies_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_proxies_deleted_at ON public.proxies USING btree (deleted_at);


--
-- Name: idx_proxies_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_proxies_status ON public.proxies USING btree (status);


--
-- Name: idx_redeem_codes_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_redeem_codes_expires_at ON public.redeem_codes USING btree (expires_at);


--
-- Name: idx_redeem_codes_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_redeem_codes_group_id ON public.redeem_codes USING btree (group_id);


--
-- Name: idx_redeem_codes_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_redeem_codes_status ON public.redeem_codes USING btree (status);


--
-- Name: idx_redeem_codes_used_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_redeem_codes_used_by ON public.redeem_codes USING btree (used_by);


--
-- Name: idx_scheduler_outbox_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_scheduler_outbox_created_at ON public.scheduler_outbox USING btree (created_at);


--
-- Name: idx_scheduler_outbox_pending_dedup_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_scheduler_outbox_pending_dedup_key ON public.scheduler_outbox USING btree (dedup_key) WHERE (dedup_key IS NOT NULL);


--
-- Name: idx_security_secrets_key; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_security_secrets_key ON public.security_secrets USING btree (key);


--
-- Name: idx_stp_account_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_stp_account_id ON public.scheduled_test_plans USING btree (account_id);


--
-- Name: idx_stp_enabled_next_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_stp_enabled_next_run ON public.scheduled_test_plans USING btree (enabled, next_run_at) WHERE (enabled = true);


--
-- Name: idx_str_plan_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_str_plan_created ON public.scheduled_test_results USING btree (plan_id, created_at DESC);


--
-- Name: idx_subscription_plans_for_sale; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_subscription_plans_for_sale ON public.subscription_plans USING btree (for_sale);


--
-- Name: idx_subscription_plans_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_subscription_plans_group_id ON public.subscription_plans USING btree (group_id);


--
-- Name: idx_ual_frozen_thaw; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ual_frozen_thaw ON public.user_affiliate_ledger USING btree (user_id, frozen_until) WHERE (frozen_until IS NOT NULL);


--
-- Name: idx_usage_billing_dedup_created_at_brin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_billing_dedup_created_at_brin ON public.usage_billing_dedup USING brin (created_at);


--
-- Name: idx_usage_billing_dedup_request_api_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_usage_billing_dedup_request_api_key ON public.usage_billing_dedup USING btree (request_id, api_key_id);


--
-- Name: idx_usage_cleanup_tasks_canceled_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_cleanup_tasks_canceled_at ON public.usage_cleanup_tasks USING btree (canceled_at DESC);


--
-- Name: idx_usage_cleanup_tasks_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_cleanup_tasks_created_at ON public.usage_cleanup_tasks USING btree (created_at DESC);


--
-- Name: idx_usage_cleanup_tasks_status_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_cleanup_tasks_status_created_at ON public.usage_cleanup_tasks USING btree (status, created_at DESC);


--
-- Name: idx_usage_dashboard_daily_bucket_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_dashboard_daily_bucket_date ON public.usage_dashboard_daily USING btree (bucket_date DESC);


--
-- Name: idx_usage_dashboard_daily_users_bucket_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_dashboard_daily_users_bucket_date ON public.usage_dashboard_daily_users USING btree (bucket_date);


--
-- Name: idx_usage_dashboard_hourly_bucket_start; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_dashboard_hourly_bucket_start ON public.usage_dashboard_hourly USING btree (bucket_start DESC);


--
-- Name: idx_usage_dashboard_hourly_users_bucket_start; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_dashboard_hourly_users_bucket_start ON public.usage_dashboard_hourly_users USING btree (bucket_start);


--
-- Name: idx_usage_logs_account_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_account_created_at ON public.usage_logs USING btree (account_id, created_at);


--
-- Name: idx_usage_logs_account_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_account_id ON public.usage_logs USING btree (account_id);


--
-- Name: idx_usage_logs_api_key_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_api_key_created_at ON public.usage_logs USING btree (api_key_id, created_at);


--
-- Name: idx_usage_logs_api_key_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_api_key_id ON public.usage_logs USING btree (api_key_id);


--
-- Name: idx_usage_logs_api_key_latest_ip; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_api_key_latest_ip ON public.usage_logs USING btree (api_key_id, created_at DESC, id DESC) INCLUDE (ip_address) WHERE ((ip_address IS NOT NULL) AND ((ip_address)::text <> ''::text));


--
-- Name: idx_usage_logs_billing_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_billing_type ON public.usage_logs USING btree (billing_type);


--
-- Name: idx_usage_logs_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_created_at ON public.usage_logs USING btree (created_at);


--
-- Name: idx_usage_logs_created_model_upstream_model; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_created_model_upstream_model ON public.usage_logs USING btree (created_at, model, upstream_model);


--
-- Name: idx_usage_logs_created_requested_model_upstream_model; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_created_requested_model_upstream_model ON public.usage_logs USING btree (created_at, requested_model, upstream_model);


--
-- Name: idx_usage_logs_effective_requested_model_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_effective_requested_model_created ON public.usage_logs USING btree (COALESCE(NULLIF(btrim((requested_model)::text), ''::text), (model)::text), created_at DESC, id DESC);


--
-- Name: idx_usage_logs_effective_upstream_model_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_effective_upstream_model_created ON public.usage_logs USING btree (COALESCE(NULLIF(btrim((upstream_model)::text), ''::text), (model)::text), created_at DESC, id DESC);


--
-- Name: idx_usage_logs_group_created_at_not_null; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_group_created_at_not_null ON public.usage_logs USING btree (group_id, created_at) WHERE (group_id IS NOT NULL);


--
-- Name: idx_usage_logs_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_group_id ON public.usage_logs USING btree (group_id);


--
-- Name: idx_usage_logs_ip_address; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_ip_address ON public.usage_logs USING btree (ip_address);


--
-- Name: idx_usage_logs_model; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_model ON public.usage_logs USING btree (model);


--
-- Name: idx_usage_logs_model_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_model_created_at ON public.usage_logs USING btree (model, created_at);


--
-- Name: idx_usage_logs_request_id_api_key_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_usage_logs_request_id_api_key_unique ON public.usage_logs USING btree (request_id, api_key_id);


--
-- Name: idx_usage_logs_request_type_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_request_type_created_at ON public.usage_logs USING btree (request_type, created_at);


--
-- Name: idx_usage_logs_service_tier_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_service_tier_created_at ON public.usage_logs USING btree (service_tier, created_at);


--
-- Name: idx_usage_logs_sub_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_sub_created ON public.usage_logs USING btree (subscription_id, created_at);


--
-- Name: idx_usage_logs_subscription_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_subscription_id ON public.usage_logs USING btree (subscription_id);


--
-- Name: idx_usage_logs_upstream_model_mismatch_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_upstream_model_mismatch_created_at ON public.usage_logs USING btree (created_at DESC, id DESC) WHERE (upstream_model_mismatch IS TRUE);


--
-- Name: idx_usage_logs_upstream_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_upstream_request_id ON public.usage_logs USING btree (upstream_request_id) WHERE (upstream_request_id IS NOT NULL);


--
-- Name: idx_usage_logs_user_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_user_created ON public.usage_logs USING btree (user_id, created_at);


--
-- Name: idx_usage_logs_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_logs_user_id ON public.usage_logs USING btree (user_id);


--
-- Name: idx_user_affiliate_ledger_action; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_affiliate_ledger_action ON public.user_affiliate_ledger USING btree (action);


--
-- Name: idx_user_affiliate_ledger_rebate_lookup; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_affiliate_ledger_rebate_lookup ON public.user_affiliate_ledger USING btree (action, source_order_id, user_id, source_user_id, created_at) WHERE ((action)::text = 'accrue'::text);


--
-- Name: idx_user_affiliate_ledger_source_order_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_affiliate_ledger_source_order_id ON public.user_affiliate_ledger USING btree (source_order_id) WHERE (source_order_id IS NOT NULL);


--
-- Name: idx_user_affiliate_ledger_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_affiliate_ledger_user_id ON public.user_affiliate_ledger USING btree (user_id);


--
-- Name: idx_user_affiliates_admin_settings; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_affiliates_admin_settings ON public.user_affiliates USING btree (updated_at) WHERE ((aff_code_custom = true) OR (aff_rebate_rate_percent IS NOT NULL));


--
-- Name: idx_user_affiliates_aff_quota; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_affiliates_aff_quota ON public.user_affiliates USING btree (aff_quota);


--
-- Name: idx_user_affiliates_inviter_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_affiliates_inviter_id ON public.user_affiliates USING btree (inviter_id);


--
-- Name: idx_user_allowed_groups_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_allowed_groups_group_id ON public.user_allowed_groups USING btree (group_id);


--
-- Name: idx_user_attribute_definitions_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_attribute_definitions_deleted_at ON public.user_attribute_definitions USING btree (deleted_at);


--
-- Name: idx_user_attribute_definitions_display_order; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_attribute_definitions_display_order ON public.user_attribute_definitions USING btree (display_order);


--
-- Name: idx_user_attribute_definitions_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_attribute_definitions_enabled ON public.user_attribute_definitions USING btree (enabled);


--
-- Name: idx_user_attribute_definitions_key_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_user_attribute_definitions_key_unique ON public.user_attribute_definitions USING btree (key) WHERE (deleted_at IS NULL);


--
-- Name: idx_user_attribute_values_attribute_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_attribute_values_attribute_id ON public.user_attribute_values USING btree (attribute_id);


--
-- Name: idx_user_attribute_values_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_attribute_values_user_id ON public.user_attribute_values USING btree (user_id);


--
-- Name: idx_user_group_rate_multipliers_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_group_rate_multipliers_group_id ON public.user_group_rate_multipliers USING btree (group_id);


--
-- Name: idx_user_subscriptions_assigned_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_subscriptions_assigned_by ON public.user_subscriptions USING btree (assigned_by);


--
-- Name: idx_user_subscriptions_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_subscriptions_expires_at ON public.user_subscriptions USING btree (expires_at);


--
-- Name: idx_user_subscriptions_group_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_subscriptions_group_id ON public.user_subscriptions USING btree (group_id);


--
-- Name: idx_user_subscriptions_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_subscriptions_status ON public.user_subscriptions USING btree (status);


--
-- Name: idx_user_subscriptions_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_subscriptions_user_id ON public.user_subscriptions USING btree (user_id);


--
-- Name: idx_user_subscriptions_user_status_expires_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_subscriptions_user_status_expires_active ON public.user_subscriptions USING btree (user_id, status, expires_at) WHERE (deleted_at IS NULL);


--
-- Name: idx_users_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_deleted_at ON public.users USING btree (deleted_at);


--
-- Name: idx_users_email_dot_stripped; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_email_dot_stripped ON public.users USING btree (replace(lower(TRIM(BOTH FROM email)), '.'::text, ''::text) text_pattern_ops) WHERE (deleted_at IS NULL);


--
-- Name: idx_users_email_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_email_trgm ON public.users USING gin (email public.gin_trgm_ops);


--
-- Name: idx_users_notes_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_notes_trgm ON public.users USING gin (notes public.gin_trgm_ops);


--
-- Name: idx_users_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_status ON public.users USING btree (status);


--
-- Name: idx_users_totp_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_totp_enabled ON public.users USING btree (totp_enabled) WHERE ((deleted_at IS NULL) AND (totp_enabled = true));


--
-- Name: idx_users_username_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_username_trgm ON public.users USING gin (username public.gin_trgm_ops);


--
-- Name: passkey_credentials_last_used_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX passkey_credentials_last_used_at_idx ON public.passkey_credentials USING btree (last_used_at);


--
-- Name: passkey_credentials_user_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX passkey_credentials_user_id_idx ON public.passkey_credentials USING btree (user_id);


--
-- Name: paymentorder_out_trade_no; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX paymentorder_out_trade_no ON public.payment_orders USING btree (out_trade_no) WHERE ((out_trade_no)::text <> ''::text);


--
-- Name: pending_auth_sessions_completion_code_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX pending_auth_sessions_completion_code_idx ON public.pending_auth_sessions USING btree (completion_code_hash);


--
-- Name: pending_auth_sessions_expires_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX pending_auth_sessions_expires_at_idx ON public.pending_auth_sessions USING btree (expires_at);


--
-- Name: pending_auth_sessions_provider_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX pending_auth_sessions_provider_idx ON public.pending_auth_sessions USING btree (provider_type, provider_key, provider_subject);


--
-- Name: pending_auth_sessions_session_token_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX pending_auth_sessions_session_token_key ON public.pending_auth_sessions USING btree (session_token);


--
-- Name: pending_auth_sessions_target_user_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX pending_auth_sessions_target_user_id_idx ON public.pending_auth_sessions USING btree (target_user_id);


--
-- Name: proxies_backup_proxy_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX proxies_backup_proxy_id_idx ON public.proxies USING btree (backup_proxy_id);


--
-- Name: proxies_expires_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX proxies_expires_at_idx ON public.proxies USING btree (expires_at);


--
-- Name: uq_accounts_spark_shadow_per_parent; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_accounts_spark_shadow_per_parent ON public.accounts USING btree (parent_account_id) WHERE ((parent_account_id IS NOT NULL) AND ((quota_dimension)::text = 'spark'::text) AND (deleted_at IS NULL));


--
-- Name: user_avatars_user_id_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX user_avatars_user_id_key ON public.user_avatars USING btree (user_id);


--
-- Name: user_provider_default_grants_user_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_provider_default_grants_user_id_idx ON public.user_provider_default_grants USING btree (user_id);


--
-- Name: user_provider_default_grants_user_provider_reason_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX user_provider_default_grants_user_provider_reason_key ON public.user_provider_default_grants USING btree (user_id, provider_type, grant_reason);


--
-- Name: user_subscriptions_user_group_unique_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX user_subscriptions_user_group_unique_active ON public.user_subscriptions USING btree (user_id, group_id) WHERE (deleted_at IS NULL);


--
-- Name: userplatformquota_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX userplatformquota_user_id ON public.user_platform_quotas USING btree (user_id);


--
-- Name: userplatformquota_user_id_platform_uq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX userplatformquota_user_id_platform_uq ON public.user_platform_quotas USING btree (user_id, platform) WHERE (deleted_at IS NULL);


--
-- Name: users_email_unique_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX users_email_unique_active ON public.users USING btree (email) WHERE (deleted_at IS NULL);


--
-- Name: usersubscription_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usersubscription_deleted_at ON public.user_subscriptions USING btree (deleted_at);


--
-- Name: accounts accounts_enforce_openai_long_context_billing_extra; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER accounts_enforce_openai_long_context_billing_extra BEFORE INSERT OR UPDATE OF platform, extra, parent_account_id, quota_dimension ON public.accounts FOR EACH ROW EXECUTE FUNCTION public.enforce_openai_long_context_billing_extra();


--
-- Name: accounts accounts_propagate_openai_long_context_billing_extra; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER accounts_propagate_openai_long_context_billing_extra AFTER UPDATE OF platform, extra ON public.accounts FOR EACH ROW WHEN ((((new.platform)::text = 'openai'::text) AND (new.parent_account_id IS NULL) AND (((old.platform)::text IS DISTINCT FROM (new.platform)::text) OR ((old.extra -> 'openai_long_context_billing_enabled'::text) IS DISTINCT FROM (new.extra -> 'openai_long_context_billing_enabled'::text))))) EXECUTE FUNCTION public.propagate_openai_long_context_billing_extra_to_shadows();


--
-- Name: api_keys trg_api_keys_auth_cache_invalidation; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_api_keys_auth_cache_invalidation AFTER DELETE OR UPDATE ON public.api_keys FOR EACH ROW EXECUTE FUNCTION public.enqueue_api_key_auth_cache_invalidation();


--
-- Name: groups trg_groups_auth_cache_invalidation; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_groups_auth_cache_invalidation AFTER DELETE OR UPDATE ON public.groups FOR EACH ROW EXECUTE FUNCTION public.enqueue_group_auth_cache_invalidation();


--
-- Name: user_allowed_groups trg_user_allowed_groups_auth_cache_invalidation; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_user_allowed_groups_auth_cache_invalidation AFTER INSERT OR DELETE OR UPDATE ON public.user_allowed_groups FOR EACH ROW EXECUTE FUNCTION public.enqueue_allowed_group_auth_cache_invalidation();


--
-- Name: users trg_users_auth_cache_invalidation; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_users_auth_cache_invalidation AFTER DELETE OR UPDATE ON public.users FOR EACH ROW EXECUTE FUNCTION public.enqueue_user_auth_cache_invalidation();


--
-- Name: usage_logs usage_logs_group_rollup_invalidate_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER usage_logs_group_rollup_invalidate_delete AFTER DELETE ON public.usage_logs FOR EACH ROW WHEN ((old.group_id IS NOT NULL)) EXECUTE FUNCTION public.invalidate_group_usage_rollup_state();


--
-- Name: usage_logs usage_logs_group_rollup_invalidate_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER usage_logs_group_rollup_invalidate_insert AFTER INSERT ON public.usage_logs REFERENCING NEW TABLE AS inserted_usage_logs FOR EACH STATEMENT EXECUTE FUNCTION public.invalidate_group_usage_rollup_state_after_insert();


--
-- Name: usage_logs usage_logs_group_rollup_invalidate_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER usage_logs_group_rollup_invalidate_update AFTER UPDATE OF created_at, group_id, actual_cost ON public.usage_logs FOR EACH ROW WHEN ((((old.created_at IS DISTINCT FROM new.created_at) OR (old.group_id IS DISTINCT FROM new.group_id) OR (old.actual_cost IS DISTINCT FROM new.actual_cost)) AND ((old.group_id IS NOT NULL) OR (new.group_id IS NOT NULL)))) EXECUTE FUNCTION public.invalidate_group_usage_rollup_state();


--
-- Name: account_groups account_groups_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_groups
    ADD CONSTRAINT account_groups_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: account_groups account_groups_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_groups
    ADD CONSTRAINT account_groups_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;


--
-- Name: accounts accounts_proxy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_proxy_id_fkey FOREIGN KEY (proxy_id) REFERENCES public.proxies(id) ON DELETE SET NULL;


--
-- Name: announcement_reads announcement_reads_announcement_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_reads
    ADD CONSTRAINT announcement_reads_announcement_id_fkey FOREIGN KEY (announcement_id) REFERENCES public.announcements(id) ON DELETE CASCADE;


--
-- Name: announcement_reads announcement_reads_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_reads
    ADD CONSTRAINT announcement_reads_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: announcements announcements_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcements
    ADD CONSTRAINT announcements_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: announcements announcements_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcements
    ADD CONSTRAINT announcements_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: api_keys api_keys_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE SET NULL;


--
-- Name: api_keys api_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: auth_identities auth_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_identities
    ADD CONSTRAINT auth_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: auth_identity_channels auth_identity_channels_identity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_identity_channels
    ADD CONSTRAINT auth_identity_channels_identity_id_fkey FOREIGN KEY (identity_id) REFERENCES public.auth_identities(id) ON DELETE CASCADE;


--
-- Name: batch_image_events batch_image_events_job_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_events
    ADD CONSTRAINT batch_image_events_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.batch_image_jobs(batch_id) ON DELETE CASCADE;


--
-- Name: batch_image_items batch_image_items_job_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batch_image_items
    ADD CONSTRAINT batch_image_items_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.batch_image_jobs(batch_id) ON DELETE CASCADE;


--
-- Name: billing_usage_entries billing_usage_entries_api_key_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_usage_entries
    ADD CONSTRAINT billing_usage_entries_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE CASCADE;


--
-- Name: billing_usage_entries billing_usage_entries_subscription_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_usage_entries
    ADD CONSTRAINT billing_usage_entries_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES public.user_subscriptions(id) ON DELETE SET NULL;


--
-- Name: billing_usage_entries billing_usage_entries_usage_log_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_usage_entries
    ADD CONSTRAINT billing_usage_entries_usage_log_id_fkey FOREIGN KEY (usage_log_id) REFERENCES public.usage_logs(id) ON DELETE CASCADE;


--
-- Name: billing_usage_entries billing_usage_entries_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_usage_entries
    ADD CONSTRAINT billing_usage_entries_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: channel_account_stats_model_pricing channel_account_stats_model_pricing_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_model_pricing
    ADD CONSTRAINT channel_account_stats_model_pricing_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.channel_account_stats_pricing_rules(id) ON DELETE CASCADE;


--
-- Name: channel_account_stats_pricing_intervals channel_account_stats_pricing_intervals_pricing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_pricing_intervals
    ADD CONSTRAINT channel_account_stats_pricing_intervals_pricing_id_fkey FOREIGN KEY (pricing_id) REFERENCES public.channel_account_stats_model_pricing(id) ON DELETE CASCADE;


--
-- Name: channel_account_stats_pricing_rules channel_account_stats_pricing_rules_channel_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_account_stats_pricing_rules
    ADD CONSTRAINT channel_account_stats_pricing_rules_channel_id_fkey FOREIGN KEY (channel_id) REFERENCES public.channels(id) ON DELETE CASCADE;


--
-- Name: channel_groups channel_groups_channel_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_groups
    ADD CONSTRAINT channel_groups_channel_id_fkey FOREIGN KEY (channel_id) REFERENCES public.channels(id) ON DELETE CASCADE;


--
-- Name: channel_groups channel_groups_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_groups
    ADD CONSTRAINT channel_groups_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;


--
-- Name: channel_model_pricing channel_model_pricing_channel_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_model_pricing
    ADD CONSTRAINT channel_model_pricing_channel_id_fkey FOREIGN KEY (channel_id) REFERENCES public.channels(id) ON DELETE CASCADE;


--
-- Name: channel_monitor_daily_rollups channel_monitor_daily_rollups_monitor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_daily_rollups
    ADD CONSTRAINT channel_monitor_daily_rollups_monitor_id_fkey FOREIGN KEY (monitor_id) REFERENCES public.channel_monitors(id) ON DELETE CASCADE;


--
-- Name: channel_monitor_histories channel_monitor_histories_monitor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitor_histories
    ADD CONSTRAINT channel_monitor_histories_monitor_id_fkey FOREIGN KEY (monitor_id) REFERENCES public.channel_monitors(id) ON DELETE CASCADE;


--
-- Name: channel_monitors channel_monitors_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitors
    ADD CONSTRAINT channel_monitors_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE SET NULL;


--
-- Name: channel_monitors channel_monitors_template_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_monitors
    ADD CONSTRAINT channel_monitors_template_id_fkey FOREIGN KEY (template_id) REFERENCES public.channel_monitor_request_templates(id) ON DELETE SET NULL;


--
-- Name: channel_pricing_intervals channel_pricing_intervals_pricing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.channel_pricing_intervals
    ADD CONSTRAINT channel_pricing_intervals_pricing_id_fkey FOREIGN KEY (pricing_id) REFERENCES public.channel_model_pricing(id) ON DELETE CASCADE;


--
-- Name: composite_model_routes composite_model_routes_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.composite_model_routes
    ADD CONSTRAINT composite_model_routes_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;


--
-- Name: content_moderation_logs content_moderation_logs_api_key_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.content_moderation_logs
    ADD CONSTRAINT content_moderation_logs_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE SET NULL;


--
-- Name: content_moderation_logs content_moderation_logs_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.content_moderation_logs
    ADD CONSTRAINT content_moderation_logs_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE SET NULL;


--
-- Name: content_moderation_logs content_moderation_logs_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.content_moderation_logs
    ADD CONSTRAINT content_moderation_logs_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: accounts fk_accounts_parent_account_id; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT fk_accounts_parent_account_id FOREIGN KEY (parent_account_id) REFERENCES public.accounts(id) ON DELETE RESTRICT;


--
-- Name: groups groups_fallback_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.groups
    ADD CONSTRAINT groups_fallback_group_id_fkey FOREIGN KEY (fallback_group_id) REFERENCES public.groups(id) ON DELETE SET NULL;


--
-- Name: groups groups_fallback_group_id_on_invalid_request_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.groups
    ADD CONSTRAINT groups_fallback_group_id_on_invalid_request_fkey FOREIGN KEY (fallback_group_id_on_invalid_request) REFERENCES public.groups(id) ON DELETE SET NULL;


--
-- Name: identity_adoption_decisions identity_adoption_decisions_identity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.identity_adoption_decisions
    ADD CONSTRAINT identity_adoption_decisions_identity_id_fkey FOREIGN KEY (identity_id) REFERENCES public.auth_identities(id) ON DELETE SET NULL;


--
-- Name: identity_adoption_decisions identity_adoption_decisions_pending_auth_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.identity_adoption_decisions
    ADD CONSTRAINT identity_adoption_decisions_pending_auth_session_id_fkey FOREIGN KEY (pending_auth_session_id) REFERENCES public.pending_auth_sessions(id) ON DELETE CASCADE;


--
-- Name: passkey_credentials passkey_credentials_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.passkey_credentials
    ADD CONSTRAINT passkey_credentials_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: passkey_user_handles passkey_user_handles_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.passkey_user_handles
    ADD CONSTRAINT passkey_user_handles_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: pending_auth_sessions pending_auth_sessions_target_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pending_auth_sessions
    ADD CONSTRAINT pending_auth_sessions_target_user_id_fkey FOREIGN KEY (target_user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: promo_code_usages promo_code_usages_promo_code_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.promo_code_usages
    ADD CONSTRAINT promo_code_usages_promo_code_id_fkey FOREIGN KEY (promo_code_id) REFERENCES public.promo_codes(id) ON DELETE CASCADE;


--
-- Name: promo_code_usages promo_code_usages_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.promo_code_usages
    ADD CONSTRAINT promo_code_usages_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: prompt_audit_events prompt_audit_events_api_key_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_events
    ADD CONSTRAINT prompt_audit_events_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE SET NULL;


--
-- Name: prompt_audit_events prompt_audit_events_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_events
    ADD CONSTRAINT prompt_audit_events_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE SET NULL;


--
-- Name: prompt_audit_events prompt_audit_events_job_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_events
    ADD CONSTRAINT prompt_audit_events_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.prompt_audit_jobs(id) ON DELETE CASCADE;


--
-- Name: prompt_audit_events prompt_audit_events_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_events
    ADD CONSTRAINT prompt_audit_events_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: prompt_audit_jobs prompt_audit_jobs_api_key_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_jobs
    ADD CONSTRAINT prompt_audit_jobs_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE SET NULL;


--
-- Name: prompt_audit_jobs prompt_audit_jobs_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_jobs
    ADD CONSTRAINT prompt_audit_jobs_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE SET NULL;


--
-- Name: prompt_audit_jobs prompt_audit_jobs_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_audit_jobs
    ADD CONSTRAINT prompt_audit_jobs_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: proxies proxies_backup_proxy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.proxies
    ADD CONSTRAINT proxies_backup_proxy_id_fkey FOREIGN KEY (backup_proxy_id) REFERENCES public.proxies(id) ON DELETE SET NULL;


--
-- Name: redeem_codes redeem_codes_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.redeem_codes
    ADD CONSTRAINT redeem_codes_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE SET NULL;


--
-- Name: redeem_codes redeem_codes_used_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.redeem_codes
    ADD CONSTRAINT redeem_codes_used_by_fkey FOREIGN KEY (used_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: scheduled_test_plans scheduled_test_plans_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduled_test_plans
    ADD CONSTRAINT scheduled_test_plans_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: scheduled_test_results scheduled_test_results_plan_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduled_test_results
    ADD CONSTRAINT scheduled_test_results_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES public.scheduled_test_plans(id) ON DELETE CASCADE;


--
-- Name: usage_cleanup_tasks usage_cleanup_tasks_canceled_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_cleanup_tasks
    ADD CONSTRAINT usage_cleanup_tasks_canceled_by_fkey FOREIGN KEY (canceled_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: usage_cleanup_tasks usage_cleanup_tasks_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_cleanup_tasks
    ADD CONSTRAINT usage_cleanup_tasks_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: usage_logs usage_logs_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_logs
    ADD CONSTRAINT usage_logs_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: usage_logs usage_logs_api_key_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_logs
    ADD CONSTRAINT usage_logs_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE CASCADE;


--
-- Name: usage_logs usage_logs_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_logs
    ADD CONSTRAINT usage_logs_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE SET NULL;


--
-- Name: usage_logs usage_logs_subscription_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_logs
    ADD CONSTRAINT usage_logs_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES public.user_subscriptions(id) ON DELETE SET NULL;


--
-- Name: usage_logs usage_logs_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_logs
    ADD CONSTRAINT usage_logs_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_affiliate_ledger user_affiliate_ledger_source_order_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliate_ledger
    ADD CONSTRAINT user_affiliate_ledger_source_order_id_fkey FOREIGN KEY (source_order_id) REFERENCES public.payment_orders(id) ON DELETE SET NULL;


--
-- Name: user_affiliate_ledger user_affiliate_ledger_source_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliate_ledger
    ADD CONSTRAINT user_affiliate_ledger_source_user_id_fkey FOREIGN KEY (source_user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: user_affiliate_ledger user_affiliate_ledger_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliate_ledger
    ADD CONSTRAINT user_affiliate_ledger_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_affiliates user_affiliates_inviter_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliates
    ADD CONSTRAINT user_affiliates_inviter_id_fkey FOREIGN KEY (inviter_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: user_affiliates user_affiliates_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_affiliates
    ADD CONSTRAINT user_affiliates_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_allowed_groups user_allowed_groups_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_allowed_groups
    ADD CONSTRAINT user_allowed_groups_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;


--
-- Name: user_allowed_groups user_allowed_groups_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_allowed_groups
    ADD CONSTRAINT user_allowed_groups_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_attribute_values user_attribute_values_attribute_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_attribute_values
    ADD CONSTRAINT user_attribute_values_attribute_id_fkey FOREIGN KEY (attribute_id) REFERENCES public.user_attribute_definitions(id) ON DELETE CASCADE;


--
-- Name: user_attribute_values user_attribute_values_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_attribute_values
    ADD CONSTRAINT user_attribute_values_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_avatars user_avatars_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_avatars
    ADD CONSTRAINT user_avatars_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_group_rate_multipliers user_group_rate_multipliers_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_group_rate_multipliers
    ADD CONSTRAINT user_group_rate_multipliers_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;


--
-- Name: user_group_rate_multipliers user_group_rate_multipliers_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_group_rate_multipliers
    ADD CONSTRAINT user_group_rate_multipliers_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_platform_quotas user_platform_quotas_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_provider_default_grants user_provider_default_grants_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_provider_default_grants
    ADD CONSTRAINT user_provider_default_grants_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: user_subscriptions user_subscriptions_assigned_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_subscriptions
    ADD CONSTRAINT user_subscriptions_assigned_by_fkey FOREIGN KEY (assigned_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: user_subscriptions user_subscriptions_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_subscriptions
    ADD CONSTRAINT user_subscriptions_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;


--
-- Name: user_subscriptions user_subscriptions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_subscriptions
    ADD CONSTRAINT user_subscriptions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- PostgreSQL database dump complete
--
