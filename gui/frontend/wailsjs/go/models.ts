export namespace app {
	
	export class UIError {
	    code: string;
	    message: string;
	    hint?: string;
	    details?: string;
	    command?: string[];
	    exit_class?: string;
	    exit_code?: number;
	    retryable: boolean;
	    doc_url?: string;
	
	    static createFrom(source: any = {}) {
	        return new UIError(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.code = source["code"];
	        this.message = source["message"];
	        this.hint = source["hint"];
	        this.details = source["details"];
	        this.command = source["command"];
	        this.exit_class = source["exit_class"];
	        this.exit_code = source["exit_code"];
	        this.retryable = source["retryable"];
	        this.doc_url = source["doc_url"];
	    }
	}
	export class AppInfo {
	    name: string;
	    version: string;
	    platform: string;
	    arch: string;
	    debark_path?: string;
	    debark_version?: string;
	    progress_events: boolean;
	    headless: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new AppInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.version = source["version"];
	        this.platform = source["platform"];
	        this.arch = source["arch"];
	        this.debark_path = source["debark_path"];
	        this.debark_version = source["debark_version"];
	        this.progress_events = source["progress_events"];
	        this.headless = source["headless"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ArchesResult {
	    arches: string[];
	    default: string;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new ArchesResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.arches = source["arches"];
	        this.default = source["default"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class BaseView {
	    id: string;
	    description?: string;
	    distro_id: string;
	    version_id: string;
	    codename: string;
	    variant?: string;
	    arch: string;
	    seeds?: string[];
	    excludes?: string[];
	    recommends: boolean;
	    digest: string;
	
	    static createFrom(source: any = {}) {
	        return new BaseView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.description = source["description"];
	        this.distro_id = source["distro_id"];
	        this.version_id = source["version_id"];
	        this.codename = source["codename"];
	        this.variant = source["variant"];
	        this.arch = source["arch"];
	        this.seeds = source["seeds"];
	        this.excludes = source["excludes"];
	        this.recommends = source["recommends"];
	        this.digest = source["digest"];
	    }
	}
	export class BasesResult {
	    arch: string;
	    bases: BaseView[];
	    caveat: string;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new BasesResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.arch = source["arch"];
	        this.bases = this.convertValues(source["bases"], BaseView);
	        this.caveat = source["caveat"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class BuildEvent {
	    seq: number;
	    ts?: string;
	    type: string;
	    level?: string;
	    msg?: string;
	    attrs?: Record<string, any>;
	    raw?: string;
	
	    static createFrom(source: any = {}) {
	        return new BuildEvent(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.ts = source["ts"];
	        this.type = source["type"];
	        this.level = source["level"];
	        this.msg = source["msg"];
	        this.attrs = source["attrs"];
	        this.raw = source["raw"];
	    }
	}
	export class BuildLogPage {
	    events: BuildEvent[];
	    next_seq: number;
	    total: number;
	    dropped: number;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new BuildLogPage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.events = this.convertValues(source["events"], BuildEvent);
	        this.next_seq = source["next_seq"];
	        this.total = source["total"];
	        this.dropped = source["dropped"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class BuildOptions {
	    output_dir: string;
	    output_name?: string;
	    format?: string;
	    signer_ref?: string;
	    no_sign?: boolean;
	    sbom?: boolean;
	    recommends?: boolean;
	    no_recommends?: boolean;
	    upgrades?: boolean;
	    update?: boolean;
	    no_prune?: boolean;
	    backend?: string;
	    acknowledge_redistribution?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new BuildOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.output_dir = source["output_dir"];
	        this.output_name = source["output_name"];
	        this.format = source["format"];
	        this.signer_ref = source["signer_ref"];
	        this.no_sign = source["no_sign"];
	        this.sbom = source["sbom"];
	        this.recommends = source["recommends"];
	        this.no_recommends = source["no_recommends"];
	        this.upgrades = source["upgrades"];
	        this.update = source["update"];
	        this.no_prune = source["no_prune"];
	        this.backend = source["backend"];
	        this.acknowledge_redistribution = source["acknowledge_redistribution"];
	    }
	}
	export class BuildProgress {
	    phase: string;
	    message?: string;
	    package?: string;
	    current: number;
	    total: number;
	    bytes_done: number;
	    bytes_total: number;
	    fraction: number;
	
	    static createFrom(source: any = {}) {
	        return new BuildProgress(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.phase = source["phase"];
	        this.message = source["message"];
	        this.package = source["package"];
	        this.current = source["current"];
	        this.total = source["total"];
	        this.bytes_done = source["bytes_done"];
	        this.bytes_total = source["bytes_total"];
	        this.fraction = source["fraction"];
	    }
	}
	export class BuildStats {
	    added: number;
	    removed: number;
	    unchanged: number;
	    package_count: number;
	    bytes: number;
	    downloaded_bytes: number;
	    duration_seconds: number;
	
	    static createFrom(source: any = {}) {
	        return new BuildStats(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.added = source["added"];
	        this.removed = source["removed"];
	        this.unchanged = source["unchanged"];
	        this.package_count = source["package_count"];
	        this.bytes = source["bytes"];
	        this.downloaded_bytes = source["downloaded_bytes"];
	        this.duration_seconds = source["duration_seconds"];
	    }
	}
	export class BuildSummary {
	    bundle_path?: string;
	    bundle_id?: string;
	    lock_ref?: string;
	    manifest_ref?: string;
	    signed: boolean;
	    stats: BuildStats;
	    warnings?: string[];
	    unresolved?: string[];
	    fetch_failed?: string[];
	    truncated: boolean;
	    exit_class?: string;
	
	    static createFrom(source: any = {}) {
	        return new BuildSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bundle_path = source["bundle_path"];
	        this.bundle_id = source["bundle_id"];
	        this.lock_ref = source["lock_ref"];
	        this.manifest_ref = source["manifest_ref"];
	        this.signed = source["signed"];
	        this.stats = this.convertValues(source["stats"], BuildStats);
	        this.warnings = source["warnings"];
	        this.unresolved = source["unresolved"];
	        this.fetch_failed = source["fetch_failed"];
	        this.truncated = source["truncated"];
	        this.exit_class = source["exit_class"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class BuildStatus {
	    target_generation: number;
	    selection_revision: number;
	    running: boolean;
	    finished: boolean;
	    cancelled: boolean;
	    progress: BuildProgress;
	    command?: string[];
	    target_id?: string;
	    item_count: number;
	    started_at?: string;
	    finished_at?: string;
	    event_count: number;
	    summary?: BuildSummary;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new BuildStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.target_generation = source["target_generation"];
	        this.selection_revision = source["selection_revision"];
	        this.running = source["running"];
	        this.finished = source["finished"];
	        this.cancelled = source["cancelled"];
	        this.progress = this.convertValues(source["progress"], BuildProgress);
	        this.command = source["command"];
	        this.target_id = source["target_id"];
	        this.item_count = source["item_count"];
	        this.started_at = source["started_at"];
	        this.finished_at = source["finished_at"];
	        this.event_count = source["event_count"];
	        this.summary = this.convertValues(source["summary"], BuildSummary);
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class CatalogProgress {
	    generation: number;
	    phase: string;
	    phase_index: number;
	    phase_count: number;
	    label: string;
	    item?: string;
	    current: number;
	    total: number;
	    bytes_done: number;
	    bytes_total: number;
	    fraction: number;
	    overall_fraction: number;
	    elapsed_ms: number;
	
	    static createFrom(source: any = {}) {
	        return new CatalogProgress(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.generation = source["generation"];
	        this.phase = source["phase"];
	        this.phase_index = source["phase_index"];
	        this.phase_count = source["phase_count"];
	        this.label = source["label"];
	        this.item = source["item"];
	        this.current = source["current"];
	        this.total = source["total"];
	        this.bytes_done = source["bytes_done"];
	        this.bytes_total = source["bytes_total"];
	        this.fraction = source["fraction"];
	        this.overall_fraction = source["overall_fraction"];
	        this.elapsed_ms = source["elapsed_ms"];
	    }
	}
	export class CatalogStatus {
	    generation: number;
	    state: string;
	    target_id?: string;
	    ready: boolean;
	    building: boolean;
	    stale: boolean;
	    progress: CatalogProgress;
	    package_count: number;
	    built_at?: string;
	    from_cache: boolean;
	    cancelled: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new CatalogStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.generation = source["generation"];
	        this.state = source["state"];
	        this.target_id = source["target_id"];
	        this.ready = source["ready"];
	        this.building = source["building"];
	        this.stale = source["stale"];
	        this.progress = this.convertValues(source["progress"], CatalogProgress);
	        this.package_count = source["package_count"];
	        this.built_at = source["built_at"];
	        this.from_cache = source["from_cache"];
	        this.cancelled = source["cancelled"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CategoryView {
	    id: string;
	    tier: string;
	    name: string;
	    count: number;
	
	    static createFrom(source: any = {}) {
	        return new CategoryView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.tier = source["tier"];
	        this.name = source["name"];
	        this.count = source["count"];
	    }
	}
	export class CategoriesResult {
	    categories: CategoryView[];
	    total: number;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new CategoriesResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.categories = this.convertValues(source["categories"], CategoryView);
	        this.total = source["total"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class CommandPreview {
	    argv: string[];
	    display: string;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new CommandPreview(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.argv = source["argv"];
	        this.display = source["display"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DestinationView {
	    destination: string;
	    path: string;
	    exists: boolean;
	    incomplete: boolean;
	    marker_name: string;
	    message?: string;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new DestinationView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.destination = source["destination"];
	        this.path = source["path"];
	        this.exists = source["exists"];
	        this.incomplete = source["incomplete"];
	        this.marker_name = source["marker_name"];
	        this.message = source["message"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ExportMismatch {
	    path: string;
	    reason: string;
	    detail?: string;
	    want_bytes: number;
	    got_bytes: number;
	    want_sha256?: string;
	    got_sha256?: string;
	
	    static createFrom(source: any = {}) {
	        return new ExportMismatch(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.reason = source["reason"];
	        this.detail = source["detail"];
	        this.want_bytes = source["want_bytes"];
	        this.got_bytes = source["got_bytes"];
	        this.want_sha256 = source["want_sha256"];
	        this.got_sha256 = source["got_sha256"];
	    }
	}
	export class ExportOptions {
	    bundle_path?: string;
	    destination: string;
	    subdir?: string;
	    skip_verify?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ExportOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bundle_path = source["bundle_path"];
	        this.destination = source["destination"];
	        this.subdir = source["subdir"];
	        this.skip_verify = source["skip_verify"];
	    }
	}
	export class ExportPlan {
	    source_dir: string;
	    dest_dir: string;
	    total_files: number;
	    total_bytes: number;
	    required_bytes: number;
	    free_bytes: number;
	    margin_bytes: number;
	    warnings?: string[];
	    destination_incomplete: boolean;
	    equivalent_command?: string[];
	
	    static createFrom(source: any = {}) {
	        return new ExportPlan(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.source_dir = source["source_dir"];
	        this.dest_dir = source["dest_dir"];
	        this.total_files = source["total_files"];
	        this.total_bytes = source["total_bytes"];
	        this.required_bytes = source["required_bytes"];
	        this.free_bytes = source["free_bytes"];
	        this.margin_bytes = source["margin_bytes"];
	        this.warnings = source["warnings"];
	        this.destination_incomplete = source["destination_incomplete"];
	        this.equivalent_command = source["equivalent_command"];
	    }
	}
	export class ExportPlanResult {
	    plan: ExportPlan;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new ExportPlanResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.plan = this.convertValues(source["plan"], ExportPlan);
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ExportProgress {
	    phase: string;
	    message?: string;
	    file?: string;
	    files_done: number;
	    files_total: number;
	    bytes_done: number;
	    bytes_total: number;
	    bytes_per_sec: number;
	    fraction: number;
	
	    static createFrom(source: any = {}) {
	        return new ExportProgress(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.phase = source["phase"];
	        this.message = source["message"];
	        this.file = source["file"];
	        this.files_done = source["files_done"];
	        this.files_total = source["files_total"];
	        this.bytes_done = source["bytes_done"];
	        this.bytes_total = source["bytes_total"];
	        this.bytes_per_sec = source["bytes_per_sec"];
	        this.fraction = source["fraction"];
	    }
	}
	export class ExportStatus {
	    running: boolean;
	    finished: boolean;
	    cancelled: boolean;
	    progress: ExportProgress;
	    bundle_path?: string;
	    destination?: string;
	    destination_path?: string;
	    started_at?: string;
	    finished_at?: string;
	    verified: boolean;
	    verify_skipped: boolean;
	    summary?: string;
	    verify_method?: string;
	    verify_caveat?: string;
	    files_checked: number;
	    bytes_checked: number;
	    mismatches?: ExportMismatch[];
	    mismatch_count: number;
	    mismatches_truncated: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new ExportStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.finished = source["finished"];
	        this.cancelled = source["cancelled"];
	        this.progress = this.convertValues(source["progress"], ExportProgress);
	        this.bundle_path = source["bundle_path"];
	        this.destination = source["destination"];
	        this.destination_path = source["destination_path"];
	        this.started_at = source["started_at"];
	        this.finished_at = source["finished_at"];
	        this.verified = source["verified"];
	        this.verify_skipped = source["verify_skipped"];
	        this.summary = source["summary"];
	        this.verify_method = source["verify_method"];
	        this.verify_caveat = source["verify_caveat"];
	        this.files_checked = source["files_checked"];
	        this.bytes_checked = source["bytes_checked"];
	        this.mismatches = this.convertValues(source["mismatches"], ExportMismatch);
	        this.mismatch_count = source["mismatch_count"];
	        this.mismatches_truncated = source["mismatches_truncated"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class FilePickResult {
	    path: string;
	    paths: string[];
	    cancelled: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new FilePickResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.paths = source["paths"];
	        this.cancelled = source["cancelled"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LifecycleView {
	    build_running: boolean;
	    export_running: boolean;
	    stopping: boolean;
	    unsaved_selection: boolean;
	    target_generation: number;
	    selection_revision: number;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new LifecycleView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.build_running = source["build_running"];
	        this.export_running = source["export_running"];
	        this.stopping = source["stopping"];
	        this.unsaved_selection = source["unsaved_selection"];
	        this.target_generation = source["target_generation"];
	        this.selection_revision = source["selection_revision"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SelectionWarning {
	    kind: string;
	    package?: string;
	    message: string;
	    hint?: string;
	    doc_url?: string;
	
	    static createFrom(source: any = {}) {
	        return new SelectionWarning(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.package = source["package"];
	        this.message = source["message"];
	        this.hint = source["hint"];
	        this.doc_url = source["doc_url"];
	    }
	}
	export class PackageDetail {
	    name: string;
	    display_name?: string;
	    version?: string;
	    suite?: string;
	    component?: string;
	    arch?: string;
	    section?: string;
	    priority?: string;
	    categories?: string[];
	    summary?: string;
	    description?: string;
	    description_truncated?: boolean;
	    installed_size_bytes: number;
	    download_size_bytes: number;
	    is_app: boolean;
	    selected: boolean;
	    source: string;
	    homepage?: string;
	    url?: string;
	    path?: string;
	    warnings?: SelectionWarning[];
	
	    static createFrom(source: any = {}) {
	        return new PackageDetail(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.display_name = source["display_name"];
	        this.version = source["version"];
	        this.suite = source["suite"];
	        this.component = source["component"];
	        this.arch = source["arch"];
	        this.section = source["section"];
	        this.priority = source["priority"];
	        this.categories = source["categories"];
	        this.summary = source["summary"];
	        this.description = source["description"];
	        this.description_truncated = source["description_truncated"];
	        this.installed_size_bytes = source["installed_size_bytes"];
	        this.download_size_bytes = source["download_size_bytes"];
	        this.is_app = source["is_app"];
	        this.selected = source["selected"];
	        this.source = source["source"];
	        this.homepage = source["homepage"];
	        this.url = source["url"];
	        this.path = source["path"];
	        this.warnings = this.convertValues(source["warnings"], SelectionWarning);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PackageResult {
	    package: PackageDetail;
	    found: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new PackageResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.package = this.convertValues(source["package"], PackageDetail);
	        this.found = source["found"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PackageRow {
	    name: string;
	    display_name?: string;
	    version?: string;
	    suite?: string;
	    component?: string;
	    arch?: string;
	    section?: string;
	    priority?: string;
	    categories?: string[];
	    summary?: string;
	    installed_size_bytes: number;
	    download_size_bytes: number;
	    is_app: boolean;
	    selected: boolean;
	    source: string;
	    warnings?: SelectionWarning[];
	
	    static createFrom(source: any = {}) {
	        return new PackageRow(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.display_name = source["display_name"];
	        this.version = source["version"];
	        this.suite = source["suite"];
	        this.component = source["component"];
	        this.arch = source["arch"];
	        this.section = source["section"];
	        this.priority = source["priority"];
	        this.categories = source["categories"];
	        this.summary = source["summary"];
	        this.installed_size_bytes = source["installed_size_bytes"];
	        this.download_size_bytes = source["download_size_bytes"];
	        this.is_app = source["is_app"];
	        this.selected = source["selected"];
	        this.source = source["source"];
	        this.warnings = this.convertValues(source["warnings"], SelectionWarning);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PackageRowsResult {
	    rows: PackageRow[];
	    missing?: string[];
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new PackageRowsResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rows = this.convertValues(source["rows"], PackageRow);
	        this.missing = source["missing"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RejectedInput {
	    value: string;
	    reason: string;
	    hint?: string;
	
	    static createFrom(source: any = {}) {
	        return new RejectedInput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.value = source["value"];
	        this.reason = source["reason"];
	        this.hint = source["hint"];
	    }
	}
	export class ParsedList {
	    count: number;
	    new_count: number;
	    known_count: number;
	    unknown?: string[];
	    unknown_count: number;
	    duplicates: number;
	    sample?: PackageRow[];
	    rejected?: RejectedInput[];
	    warnings?: SelectionWarning[];
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new ParsedList(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.count = source["count"];
	        this.new_count = source["new_count"];
	        this.known_count = source["known_count"];
	        this.unknown = source["unknown"];
	        this.unknown_count = source["unknown_count"];
	        this.duplicates = source["duplicates"];
	        this.sample = this.convertValues(source["sample"], PackageRow);
	        this.rejected = this.convertValues(source["rejected"], RejectedInput);
	        this.warnings = this.convertValues(source["warnings"], SelectionWarning);
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ReadinessAction {
	    label: string;
	    command: string[];
	    display: string;
	    elevated: boolean;
	    runnable: boolean;
	    note?: string;
	
	    static createFrom(source: any = {}) {
	        return new ReadinessAction(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.label = source["label"];
	        this.command = source["command"];
	        this.display = source["display"];
	        this.elevated = source["elevated"];
	        this.runnable = source["runnable"];
	        this.note = source["note"];
	    }
	}
	export class ReadinessCheck {
	    id: string;
	    title: string;
	    status: string;
	    severity: string;
	    summary: string;
	    remedy?: string;
	    action?: ReadinessAction;
	    detail?: string;
	    duration_ms: number;
	    running: boolean;
	    derived: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ReadinessCheck(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.status = source["status"];
	        this.severity = source["severity"];
	        this.summary = source["summary"];
	        this.remedy = source["remedy"];
	        this.action = this.convertValues(source["action"], ReadinessAction);
	        this.detail = source["detail"];
	        this.duration_ms = source["duration_ms"];
	        this.running = source["running"];
	        this.derived = source["derived"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RemoteBuilderAdvice {
	    reason: string;
	    message: string;
	    hint: string;
	    blocked?: string[];
	
	    static createFrom(source: any = {}) {
	        return new RemoteBuilderAdvice(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.reason = source["reason"];
	        this.message = source["message"];
	        this.hint = source["hint"];
	        this.blocked = source["blocked"];
	    }
	}
	export class ReadinessReport {
	    checks: ReadinessCheck[];
	    can_build: boolean;
	    blocking_count: number;
	    degraded_count: number;
	    platform?: string;
	    checking: boolean;
	    running_check?: string;
	    checked_at?: string;
	    duration_ms: number;
	    remote_builder?: RemoteBuilderAdvice;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new ReadinessReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.checks = this.convertValues(source["checks"], ReadinessCheck);
	        this.can_build = source["can_build"];
	        this.blocking_count = source["blocking_count"];
	        this.degraded_count = source["degraded_count"];
	        this.platform = source["platform"];
	        this.checking = source["checking"];
	        this.running_check = source["running_check"];
	        this.checked_at = source["checked_at"];
	        this.duration_ms = source["duration_ms"];
	        this.remote_builder = this.convertValues(source["remote_builder"], RemoteBuilderAdvice);
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	
	export class Result {
	    ok: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new Result(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SearchQuery {
	    text: string;
	    category?: string;
	    offset: number;
	    limit: number;
	    apps_only?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SearchQuery(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.text = source["text"];
	        this.category = source["category"];
	        this.offset = source["offset"];
	        this.limit = source["limit"];
	        this.apps_only = source["apps_only"];
	    }
	}
	export class SearchResult {
	    rows: PackageRow[];
	    total: number;
	    offset: number;
	    limit: number;
	    query: SearchQuery;
	    took_ms: number;
	    truncated: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new SearchResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rows = this.convertValues(source["rows"], PackageRow);
	        this.total = source["total"];
	        this.offset = source["offset"];
	        this.limit = source["limit"];
	        this.query = this.convertValues(source["query"], SearchQuery);
	        this.took_ms = source["took_ms"];
	        this.truncated = source["truncated"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SelectionEntry {
	    key: string;
	    name: string;
	    display_name?: string;
	    version?: string;
	    summary?: string;
	    source: string;
	    url?: string;
	    path?: string;
	    installed_size_bytes: number;
	    download_size_bytes: number;
	    known: boolean;
	    sha256?: string;
	    warnings?: SelectionWarning[];
	
	    static createFrom(source: any = {}) {
	        return new SelectionEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.name = source["name"];
	        this.display_name = source["display_name"];
	        this.version = source["version"];
	        this.summary = source["summary"];
	        this.source = source["source"];
	        this.url = source["url"];
	        this.path = source["path"];
	        this.installed_size_bytes = source["installed_size_bytes"];
	        this.download_size_bytes = source["download_size_bytes"];
	        this.known = source["known"];
	        this.sha256 = source["sha256"];
	        this.warnings = this.convertValues(source["warnings"], SelectionWarning);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SelectionKeysResult {
	    keys: string[];
	    total: number;
	    revision: number;
	    truncated: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new SelectionKeysResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.keys = source["keys"];
	        this.total = source["total"];
	        this.revision = source["revision"];
	        this.truncated = source["truncated"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SelectionPage {
	    entries: SelectionEntry[];
	    total: number;
	    offset: number;
	    limit: number;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new SelectionPage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.entries = this.convertValues(source["entries"], SelectionEntry);
	        this.total = source["total"];
	        this.offset = source["offset"];
	        this.limit = source["limit"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SelectionSummary {
	    undo_token: string;
	    undo_label: string;
	    total: number;
	    package_count: number;
	    url_count: number;
	    file_count: number;
	    unknown_count: number;
	    installed_size_bytes: number;
	    download_size_bytes: number;
	    warnings?: SelectionWarning[];
	    rejected?: RejectedInput[];
	    added: number;
	    removed: number;
	    revision: number;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new SelectionSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.undo_token = source["undo_token"];
	        this.undo_label = source["undo_label"];
	        this.total = source["total"];
	        this.package_count = source["package_count"];
	        this.url_count = source["url_count"];
	        this.file_count = source["file_count"];
	        this.unknown_count = source["unknown_count"];
	        this.installed_size_bytes = source["installed_size_bytes"];
	        this.download_size_bytes = source["download_size_bytes"];
	        this.warnings = this.convertValues(source["warnings"], SelectionWarning);
	        this.rejected = this.convertValues(source["rejected"], RejectedInput);
	        this.added = source["added"];
	        this.removed = source["removed"];
	        this.revision = source["revision"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class SnapshotView {
	    path: string;
	    schema_version?: string;
	    distro_id: string;
	    version_id: string;
	    codename: string;
	    variant?: string;
	    arch: string;
	    origin_kind?: string;
	    created_at?: string;
	    package_count: number;
	    foreign_archs?: string[];
	    base_id?: string;
	    origin_source?: string;
	    origin_source_digest?: string;
	
	    static createFrom(source: any = {}) {
	        return new SnapshotView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.schema_version = source["schema_version"];
	        this.distro_id = source["distro_id"];
	        this.version_id = source["version_id"];
	        this.codename = source["codename"];
	        this.variant = source["variant"];
	        this.arch = source["arch"];
	        this.origin_kind = source["origin_kind"];
	        this.created_at = source["created_at"];
	        this.package_count = source["package_count"];
	        this.foreign_archs = source["foreign_archs"];
	        this.base_id = source["base_id"];
	        this.origin_source = source["origin_source"];
	        this.origin_source_digest = source["origin_source_digest"];
	    }
	}
	export class SnapshotResult {
	    snapshot: SnapshotView;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new SnapshotResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.snapshot = this.convertValues(source["snapshot"], SnapshotView);
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class SourceProblemView {
	    kind: string;
	    deliberate: boolean;
	    file?: string;
	    line?: number;
	    reason: string;
	    text?: string;
	
	    static createFrom(source: any = {}) {
	        return new SourceProblemView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.deliberate = source["deliberate"];
	        this.file = source["file"];
	        this.line = source["line"];
	        this.reason = source["reason"];
	        this.text = source["text"];
	    }
	}
	export class TargetView {
	    generation: number;
	    selected: boolean;
	    kind?: string;
	    id?: string;
	    label?: string;
	    distro_id?: string;
	    version_id?: string;
	    codename?: string;
	    variant?: string;
	    arch?: string;
	    snapshot_path?: string;
	    assumed: boolean;
	    caveat?: string;
	    components?: string[];
	    suites?: string[];
	    catalog_ready: boolean;
	
	    static createFrom(source: any = {}) {
	        return new TargetView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.generation = source["generation"];
	        this.selected = source["selected"];
	        this.kind = source["kind"];
	        this.id = source["id"];
	        this.label = source["label"];
	        this.distro_id = source["distro_id"];
	        this.version_id = source["version_id"];
	        this.codename = source["codename"];
	        this.variant = source["variant"];
	        this.arch = source["arch"];
	        this.snapshot_path = source["snapshot_path"];
	        this.assumed = source["assumed"];
	        this.caveat = source["caveat"];
	        this.components = source["components"];
	        this.suites = source["suites"];
	        this.catalog_ready = source["catalog_ready"];
	    }
	}
	export class TargetResult {
	    target: TargetView;
	    error?: UIError;
	    source_problems?: SourceProblemView[];
	
	    static createFrom(source: any = {}) {
	        return new TargetResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.target = this.convertValues(source["target"], TargetView);
	        this.error = this.convertValues(source["error"], UIError);
	        this.source_problems = this.convertValues(source["source_problems"], SourceProblemView);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TargetSelection {
	    kind: string;
	    base_id?: string;
	    arch?: string;
	    snapshot_path?: string;
	
	    static createFrom(source: any = {}) {
	        return new TargetSelection(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.base_id = source["base_id"];
	        this.arch = source["arch"];
	        this.snapshot_path = source["snapshot_path"];
	    }
	}
	
	
	export class URLInput {
	    url: string;
	    sha256?: string;
	
	    static createFrom(source: any = {}) {
	        return new URLInput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.sha256 = source["sha256"];
	    }
	}
	export class VerifyFinding {
	    severity: string;
	    code?: string;
	    message: string;
	    hint?: string;
	    path?: string;
	
	    static createFrom(source: any = {}) {
	        return new VerifyFinding(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.severity = source["severity"];
	        this.code = source["code"];
	        this.message = source["message"];
	        this.hint = source["hint"];
	        this.path = source["path"];
	    }
	}
	export class VerifyStatus {
	    running: boolean;
	    finished: boolean;
	    cancelled: boolean;
	    bundle_path?: string;
	    ok: boolean;
	    signed: boolean;
	    findings?: VerifyFinding[];
	    exit_class?: string;
	    started_at?: string;
	    finished_at?: string;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new VerifyStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.finished = source["finished"];
	        this.cancelled = source["cancelled"];
	        this.bundle_path = source["bundle_path"];
	        this.ok = source["ok"];
	        this.signed = source["signed"];
	        this.findings = this.convertValues(source["findings"], VerifyFinding);
	        this.exit_class = source["exit_class"];
	        this.started_at = source["started_at"];
	        this.finished_at = source["finished_at"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class VolumeView {
	    path: string;
	    label: string;
	    device?: string;
	    fs_type?: string;
	    total_bytes: number;
	    free_bytes: number;
	    kind: string;
	    removable: boolean;
	    read_only: boolean;
	    writable: boolean;
	    note?: string;
	
	    static createFrom(source: any = {}) {
	        return new VolumeView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.label = source["label"];
	        this.device = source["device"];
	        this.fs_type = source["fs_type"];
	        this.total_bytes = source["total_bytes"];
	        this.free_bytes = source["free_bytes"];
	        this.kind = source["kind"];
	        this.removable = source["removable"];
	        this.read_only = source["read_only"];
	        this.writable = source["writable"];
	        this.note = source["note"];
	    }
	}
	export class VolumesResult {
	    volumes: VolumeView[];
	    fingerprint?: string;
	    supported: boolean;
	    error?: UIError;
	
	    static createFrom(source: any = {}) {
	        return new VolumesResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.volumes = this.convertValues(source["volumes"], VolumeView);
	        this.fingerprint = source["fingerprint"];
	        this.supported = source["supported"];
	        this.error = this.convertValues(source["error"], UIError);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

