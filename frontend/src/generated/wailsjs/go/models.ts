export namespace analysis {

	export class ASRStatusDTO {
	    version: number;
	    providerId: string;
	    state: string;
	    configured: boolean;
	    available: boolean;
	    errorCode?: string;

	    static createFrom(source: any = {}) {
	        return new ASRStatusDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.providerId = source["providerId"];
	        this.state = source["state"];
	        this.configured = source["configured"];
	        this.available = source["available"];
	        this.errorCode = source["errorCode"];
	    }
	}
	export class AnalyzeRequest {
	    sessionId: string;

	    static createFrom(source: any = {}) {
	        return new AnalyzeRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sessionId = source["sessionId"];
	    }
	}
	export class MetricContributionDTO {
	    metric: string;
	    weight: number;
	    score: number;

	    static createFrom(source: any = {}) {
	        return new MetricContributionDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.metric = source["metric"];
	        this.weight = source["weight"];
	        this.score = source["score"];
	    }
	}
	export class CandidateDTO {
	    id: string;
	    kind: string;
	    startMs: number;
	    endMs: number;
	    score: number;
	    threshold: number;
	    baselineMedian: number;
	    baselineMad: number;
	    completeness: number;
	    contributions: MetricContributionDTO[];
	    evidenceBucketMs: number[];
	    algorithmVersion: string;
	    sourceCandidateId?: string;

	    static createFrom(source: any = {}) {
	        return new CandidateDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.startMs = source["startMs"];
	        this.endMs = source["endMs"];
	        this.score = source["score"];
	        this.threshold = source["threshold"];
	        this.baselineMedian = source["baselineMedian"];
	        this.baselineMad = source["baselineMad"];
	        this.completeness = source["completeness"];
	        this.contributions = this.convertValues(source["contributions"], MetricContributionDTO);
	        this.evidenceBucketMs = source["evidenceBucketMs"];
	        this.algorithmVersion = source["algorithmVersion"];
	        this.sourceCandidateId = source["sourceCandidateId"];
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
	export class ExportFileDTO {
	    name: string;
	    mediaType: string;
	    rowCount: number;
	    sizeBytes: number;
	    sha256: string;

	    static createFrom(source: any = {}) {
	        return new ExportFileDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.mediaType = source["mediaType"];
	        this.rowCount = source["rowCount"];
	        this.sizeBytes = source["sizeBytes"];
	        this.sha256 = source["sha256"];
	    }
	}
	export class ExportRequest {
	    sessionId: string;
	    includeText: boolean;

	    static createFrom(source: any = {}) {
	        return new ExportRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sessionId = source["sessionId"];
	        this.includeText = source["includeText"];
	    }
	}
	export class ExportResultDTO {
	    version: number;
	    exportId: string;
	    directoryName: string;
	    generatedAt: string;
	    includeText: boolean;
	    files: ExportFileDTO[];

	    static createFrom(source: any = {}) {
	        return new ExportResultDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.exportId = source["exportId"];
	        this.directoryName = source["directoryName"];
	        this.generatedAt = source["generatedAt"];
	        this.includeText = source["includeText"];
	        this.files = this.convertValues(source["files"], ExportFileDTO);
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
	export class MetricBucketDTO {
	    bucketStartMs: number;
	    bucketSizeMs: number;
	    chatCount: number;
	    uniqueChatters: number;
	    likeDelta: number;
	    giftCount: number;
	    giftValue?: number;
	    followCount: number;
	    enterCount: number;
	    activeUsers: number;
	    messageTotal: number;
	    completeness: number;

	    static createFrom(source: any = {}) {
	        return new MetricBucketDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bucketStartMs = source["bucketStartMs"];
	        this.bucketSizeMs = source["bucketSizeMs"];
	        this.chatCount = source["chatCount"];
	        this.uniqueChatters = source["uniqueChatters"];
	        this.likeDelta = source["likeDelta"];
	        this.giftCount = source["giftCount"];
	        this.giftValue = source["giftValue"];
	        this.followCount = source["followCount"];
	        this.enterCount = source["enterCount"];
	        this.activeUsers = source["activeUsers"];
	        this.messageTotal = source["messageTotal"];
	        this.completeness = source["completeness"];
	    }
	}

	export class MetricTotalsDTO {
	    chatCount: number;
	    uniqueChatters: number;
	    likeDelta: number;
	    giftCount: number;
	    giftValue?: number;
	    followCount: number;
	    enterCount: number;
	    activeUsers: number;
	    messageTotal: number;

	    static createFrom(source: any = {}) {
	        return new MetricTotalsDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.chatCount = source["chatCount"];
	        this.uniqueChatters = source["uniqueChatters"];
	        this.likeDelta = source["likeDelta"];
	        this.giftCount = source["giftCount"];
	        this.giftValue = source["giftValue"];
	        this.followCount = source["followCount"];
	        this.enterCount = source["enterCount"];
	        this.activeUsers = source["activeUsers"];
	        this.messageTotal = source["messageTotal"];
	    }
	}
	export class SummaryDTO {
	    durationMs: number;
	    bucketSizeMs: number;
	    bucketCount: number;
	    completeness: number;
	    totals: MetricTotalsDTO;
	    peakCount: number;
	    troughCount: number;
	    highlightCount: number;
	    warnings: string[];

	    static createFrom(source: any = {}) {
	        return new SummaryDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.durationMs = source["durationMs"];
	        this.bucketSizeMs = source["bucketSizeMs"];
	        this.bucketCount = source["bucketCount"];
	        this.completeness = source["completeness"];
	        this.totals = this.convertValues(source["totals"], MetricTotalsDTO);
	        this.peakCount = source["peakCount"];
	        this.troughCount = source["troughCount"];
	        this.highlightCount = source["highlightCount"];
	        this.warnings = source["warnings"];
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
	export class ReportDTO {
	    version: number;
	    id: string;
	    sessionId: string;
	    status: string;
	    analysisVersion: string;
	    algorithmVersion: string;
	    startedAt: number;
	    completedAt: number;
	    summary: SummaryDTO;
	    buckets: MetricBucketDTO[];
	    peaks: CandidateDTO[];
	    troughs: CandidateDTO[];
	    highlights: CandidateDTO[];

	    static createFrom(source: any = {}) {
	        return new ReportDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.id = source["id"];
	        this.sessionId = source["sessionId"];
	        this.status = source["status"];
	        this.analysisVersion = source["analysisVersion"];
	        this.algorithmVersion = source["algorithmVersion"];
	        this.startedAt = source["startedAt"];
	        this.completedAt = source["completedAt"];
	        this.summary = this.convertValues(source["summary"], SummaryDTO);
	        this.buckets = this.convertValues(source["buckets"], MetricBucketDTO);
	        this.peaks = this.convertValues(source["peaks"], CandidateDTO);
	        this.troughs = this.convertValues(source["troughs"], CandidateDTO);
	        this.highlights = this.convertValues(source["highlights"], CandidateDTO);
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

export namespace app {

	export class CapabilityDTO {
	    id: string;
	    label: string;
	    available: boolean;

	    static createFrom(source: any = {}) {
	        return new CapabilityDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.label = source["label"];
	        this.available = source["available"];
	    }
	}
	export class DataStatusDTO {
	    ready: boolean;
	    schemaVersion: number;
	    mode: string;
	    loggingReady: boolean;

	    static createFrom(source: any = {}) {
	        return new DataStatusDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ready = source["ready"];
	        this.schemaVersion = source["schemaVersion"];
	        this.mode = source["mode"];
	        this.loggingReady = source["loggingReady"];
	    }
	}
	export class BootstrapDTO {
	    apiVersion: string;
	    name: string;
	    version: string;
	    state: string;
	    data: DataStatusDTO;
	    capabilities: CapabilityDTO[];

	    static createFrom(source: any = {}) {
	        return new BootstrapDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.apiVersion = source["apiVersion"];
	        this.name = source["name"];
	        this.version = source["version"];
	        this.state = source["state"];
	        this.data = this.convertValues(source["data"], DataStatusDTO);
	        this.capabilities = this.convertValues(source["capabilities"], CapabilityDTO);
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

export namespace playback {

	export class EventDTO {
	    id: string;
	    ingestSequence: number;
	    role: string;
	    kind: string;
	    receivedAt: number;
	    sessionOffsetMs: number;
	    clockConfidence: number;
	    displayName?: string;
	    content?: string;
	    numericValue?: number;
	    parseStatus: string;

	    static createFrom(source: any = {}) {
	        return new EventDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.ingestSequence = source["ingestSequence"];
	        this.role = source["role"];
	        this.kind = source["kind"];
	        this.receivedAt = source["receivedAt"];
	        this.sessionOffsetMs = source["sessionOffsetMs"];
	        this.clockConfidence = source["clockConfidence"];
	        this.displayName = source["displayName"];
	        this.content = source["content"];
	        this.numericValue = source["numericValue"];
	        this.parseStatus = source["parseStatus"];
	    }
	}
	export class EventFilter {
	    SessionID: string;
	    Kinds: string[];
	    Roles: string[];
	    OffsetMin?: number;
	    OffsetMax?: number;

	    static createFrom(source: any = {}) {
	        return new EventFilter(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.SessionID = source["SessionID"];
	        this.Kinds = source["Kinds"];
	        this.Roles = source["Roles"];
	        this.OffsetMin = source["OffsetMin"];
	        this.OffsetMax = source["OffsetMax"];
	    }
	}
	export class EventPage {
	    version: number;
	    items: EventDTO[];
	    nextCursor?: string;

	    static createFrom(source: any = {}) {
	        return new EventPage(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.items = this.convertValues(source["items"], EventDTO);
	        this.nextCursor = source["nextCursor"];
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
	export class GapDTO {
	    id: string;
	    kind: string;
	    startedAt: number;
	    endedAt?: number;
	    startOffsetMs: number;
	    endOffsetMs?: number;
	    severity: string;
	    recovered: boolean;
	    reasonCode: string;

	    static createFrom(source: any = {}) {
	        return new GapDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.startedAt = source["startedAt"];
	        this.endedAt = source["endedAt"];
	        this.startOffsetMs = source["startOffsetMs"];
	        this.endOffsetMs = source["endOffsetMs"];
	        this.severity = source["severity"];
	        this.recovered = source["recovered"];
	        this.reasonCode = source["reasonCode"];
	    }
	}
	export class GapFilter {
	    SessionID: string;
	    Kinds: string[];
	    Recovered?: boolean;
	    OffsetMin?: number;
	    OffsetMax?: number;

	    static createFrom(source: any = {}) {
	        return new GapFilter(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.SessionID = source["SessionID"];
	        this.Kinds = source["Kinds"];
	        this.Recovered = source["Recovered"];
	        this.OffsetMin = source["OffsetMin"];
	        this.OffsetMax = source["OffsetMax"];
	    }
	}
	export class GapPage {
	    version: number;
	    items: GapDTO[];
	    nextCursor?: string;

	    static createFrom(source: any = {}) {
	        return new GapPage(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.items = this.convertValues(source["items"], GapDTO);
	        this.nextCursor = source["nextCursor"];
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
	export class MediaArtifactDTO {
	    id: string;
	    mediaSegmentId: string;
	    kind: string;
	    container: string;
	    codec: string;
	    durationMs: number;
	    sizeBytes: number;
	    sampleRate: number;
	    channels: number;
	    status: string;
	    errorCode?: string;
	    directPlayback: boolean;

	    static createFrom(source: any = {}) {
	        return new MediaArtifactDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.mediaSegmentId = source["mediaSegmentId"];
	        this.kind = source["kind"];
	        this.container = source["container"];
	        this.codec = source["codec"];
	        this.durationMs = source["durationMs"];
	        this.sizeBytes = source["sizeBytes"];
	        this.sampleRate = source["sampleRate"];
	        this.channels = source["channels"];
	        this.status = source["status"];
	        this.errorCode = source["errorCode"];
	        this.directPlayback = source["directPlayback"];
	    }
	}
	export class MediaFilter {
	    SessionID: string;
	    Statuses: string[];

	    static createFrom(source: any = {}) {
	        return new MediaFilter(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.SessionID = source["SessionID"];
	        this.Statuses = source["Statuses"];
	    }
	}
	export class MediaLocationRequest {
	    SessionID: string;
	    SessionOffsetMS: number;

	    static createFrom(source: any = {}) {
	        return new MediaLocationRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.SessionID = source["SessionID"];
	        this.SessionOffsetMS = source["SessionOffsetMS"];
	    }
	}
	export class MediaSegmentDTO {
	    id: string;
	    sequence: number;
	    container: string;
	    videoCodec?: string;
	    audioCodec?: string;
	    startedAt: number;
	    endedAt: number;
	    ptsStartMs?: number;
	    ptsEndMs?: number;
	    durationMs: number;
	    sizeBytes: number;
	    status: string;
	    errorCode?: string;
	    timelineStartMs: number;
	    timelineEndMs: number;
	    artifacts: MediaArtifactDTO[];
	    playbackArtifactId?: string;

	    static createFrom(source: any = {}) {
	        return new MediaSegmentDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.sequence = source["sequence"];
	        this.container = source["container"];
	        this.videoCodec = source["videoCodec"];
	        this.audioCodec = source["audioCodec"];
	        this.startedAt = source["startedAt"];
	        this.endedAt = source["endedAt"];
	        this.ptsStartMs = source["ptsStartMs"];
	        this.ptsEndMs = source["ptsEndMs"];
	        this.durationMs = source["durationMs"];
	        this.sizeBytes = source["sizeBytes"];
	        this.status = source["status"];
	        this.errorCode = source["errorCode"];
	        this.timelineStartMs = source["timelineStartMs"];
	        this.timelineEndMs = source["timelineEndMs"];
	        this.artifacts = this.convertValues(source["artifacts"], MediaArtifactDTO);
	        this.playbackArtifactId = source["playbackArtifactId"];
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
	export class MediaLocationResult {
	    version: number;
	    sessionId: string;
	    requestedOffsetMs: number;
	    adjustedOffsetMs: number;
	    state: string;
	    reasonCode?: string;
	    segment?: MediaSegmentDTO;
	    segmentPlaybackMs?: number;
	    playbackArtifactId?: string;

	    static createFrom(source: any = {}) {
	        return new MediaLocationResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.sessionId = source["sessionId"];
	        this.requestedOffsetMs = source["requestedOffsetMs"];
	        this.adjustedOffsetMs = source["adjustedOffsetMs"];
	        this.state = source["state"];
	        this.reasonCode = source["reasonCode"];
	        this.segment = this.convertValues(source["segment"], MediaSegmentDTO);
	        this.segmentPlaybackMs = source["segmentPlaybackMs"];
	        this.playbackArtifactId = source["playbackArtifactId"];
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
	export class MediaPage {
	    version: number;
	    items: MediaSegmentDTO[];
	    nextCursor?: string;

	    static createFrom(source: any = {}) {
	        return new MediaPage(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.items = this.convertValues(source["items"], MediaSegmentDTO);
	        this.nextCursor = source["nextCursor"];
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

	export class PageRequest {
	    Limit: number;
	    Cursor: string;

	    static createFrom(source: any = {}) {
	        return new PageRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Limit = source["Limit"];
	        this.Cursor = source["Cursor"];
	    }
	}
	export class SessionDTO {
	    id: string;
	    roomConfigId: string;
	    roomAlias: string;
	    title: string;
	    status: string;
	    recordingStatus: string;
	    startedAt: number;
	    endedAt?: number;
	    mediaEpochAt?: number;
	    captureOffsetMs: number;
	    clockSource: string;
	    integrityScore: number;
	    sessionMediaState?: string;

	    static createFrom(source: any = {}) {
	        return new SessionDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.roomConfigId = source["roomConfigId"];
	        this.roomAlias = source["roomAlias"];
	        this.title = source["title"];
	        this.status = source["status"];
	        this.recordingStatus = source["recordingStatus"];
	        this.startedAt = source["startedAt"];
	        this.endedAt = source["endedAt"];
	        this.mediaEpochAt = source["mediaEpochAt"];
	        this.captureOffsetMs = source["captureOffsetMs"];
	        this.clockSource = source["clockSource"];
	        this.integrityScore = source["integrityScore"];
	        this.sessionMediaState = source["sessionMediaState"];
	    }
	}
	export class SessionFilter {
	    RoomConfigID: string;
	    Statuses: string[];
	    StartedAtMin?: number;
	    StartedAtMax?: number;

	    static createFrom(source: any = {}) {
	        return new SessionFilter(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.RoomConfigID = source["RoomConfigID"];
	        this.Statuses = source["Statuses"];
	        this.StartedAtMin = source["StartedAtMin"];
	        this.StartedAtMax = source["StartedAtMax"];
	    }
	}
	export class SessionPage {
	    version: number;
	    items: SessionDTO[];
	    nextCursor?: string;

	    static createFrom(source: any = {}) {
	        return new SessionPage(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.items = this.convertValues(source["items"], SessionDTO);
	        this.nextCursor = source["nextCursor"];
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
	export class SessionResult {
	    version: number;
	    session: SessionDTO;

	    static createFrom(source: any = {}) {
	        return new SessionResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.session = this.convertValues(source["session"], SessionDTO);
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

export namespace room {

	export class CookieStatus {
	    configured: boolean;
	    updatedAt?: number;

	    static createFrom(source: any = {}) {
	        return new CookieStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.configured = source["configured"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	export class RecordingProfile {
	    quality: string;
	    segmentMinutes: number;
	    saveDirectory?: string;

	    static createFrom(source: any = {}) {
	        return new RecordingProfile(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.quality = source["quality"];
	        this.segmentMinutes = source["segmentMinutes"];
	        this.saveDirectory = source["saveDirectory"];
	    }
	}
	export class CreateRoomInput {
	    liveId: string;
	    alias: string;
	    monitorEnabled: boolean;
	    recordEnabled: boolean;
	    recordingProfile: RecordingProfile;

	    static createFrom(source: any = {}) {
	        return new CreateRoomInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.liveId = source["liveId"];
	        this.alias = source["alias"];
	        this.monitorEnabled = source["monitorEnabled"];
	        this.recordEnabled = source["recordEnabled"];
	        this.recordingProfile = this.convertValues(source["recordingProfile"], RecordingProfile);
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

	export class RoomConfig {
	    id: string;
	    liveId: string;
	    roomId?: string;
	    alias: string;
	    anchorName?: string;
	    monitorEnabled: boolean;
	    recordEnabled: boolean;
	    recordingProfile: RecordingProfile;
	    cookie: CookieStatus;
	    createdAt: number;
	    updatedAt: number;

	    static createFrom(source: any = {}) {
	        return new RoomConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.liveId = source["liveId"];
	        this.roomId = source["roomId"];
	        this.alias = source["alias"];
	        this.anchorName = source["anchorName"];
	        this.monitorEnabled = source["monitorEnabled"];
	        this.recordEnabled = source["recordEnabled"];
	        this.recordingProfile = this.convertValues(source["recordingProfile"], RecordingProfile);
	        this.cookie = this.convertValues(source["cookie"], CookieStatus);
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
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
	export class RoomRuntimeStatus {
	    roomId: string;
	    liveId: string;
	    alias: string;
	    state: string;
	    operationId?: string;
	    sessionId?: string;
	    recordingStatus?: string;
	    liveName?: string;
	    title?: string;
	    lastCheckedAt?: number;
	    changedAt: number;
	    revision: number;
	    retryAt?: number;
	    errorCode?: string;
	    message: string;

	    static createFrom(source: any = {}) {
	        return new RoomRuntimeStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.roomId = source["roomId"];
	        this.liveId = source["liveId"];
	        this.alias = source["alias"];
	        this.state = source["state"];
	        this.operationId = source["operationId"];
	        this.sessionId = source["sessionId"];
	        this.recordingStatus = source["recordingStatus"];
	        this.liveName = source["liveName"];
	        this.title = source["title"];
	        this.lastCheckedAt = source["lastCheckedAt"];
	        this.changedAt = source["changedAt"];
	        this.revision = source["revision"];
	        this.retryAt = source["retryAt"];
	        this.errorCode = source["errorCode"];
	        this.message = source["message"];
	    }
	}
	export class SetRoomCookieInput {
	    roomId: string;
	    cookie: string;

	    static createFrom(source: any = {}) {
	        return new SetRoomCookieInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.roomId = source["roomId"];
	        this.cookie = source["cookie"];
	    }
	}
	export class UpdateRoomInput {
	    liveId: string;
	    alias: string;
	    monitorEnabled: boolean;
	    recordEnabled: boolean;
	    recordingProfile: RecordingProfile;

	    static createFrom(source: any = {}) {
	        return new UpdateRoomInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.liveId = source["liveId"];
	        this.alias = source["alias"];
	        this.monitorEnabled = source["monitorEnabled"];
	        this.recordEnabled = source["recordEnabled"];
	        this.recordingProfile = this.convertValues(source["recordingProfile"], RecordingProfile);
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

export namespace settings {

	export class AppSettings {
	    version: number;
	    storageRoot: string;
	    recordingDirectory: string;
	    defaultQuality: string;
	    defaultSegmentMinutes: number;
	    maxConcurrentRecordings: number;
	    minimumFreeSpaceGiB: number;
	    saveDisplayNames: boolean;

	    static createFrom(source: any = {}) {
	        return new AppSettings(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.storageRoot = source["storageRoot"];
	        this.recordingDirectory = source["recordingDirectory"];
	        this.defaultQuality = source["defaultQuality"];
	        this.defaultSegmentMinutes = source["defaultSegmentMinutes"];
	        this.maxConcurrentRecordings = source["maxConcurrentRecordings"];
	        this.minimumFreeSpaceGiB = source["minimumFreeSpaceGiB"];
	        this.saveDisplayNames = source["saveDisplayNames"];
	    }
	}
	export class UpdateSettingsInput {
	    recordingDirectory: string;
	    defaultQuality: string;
	    defaultSegmentMinutes: number;
	    maxConcurrentRecordings: number;
	    minimumFreeSpaceGiB: number;
	    saveDisplayNames: boolean;

	    static createFrom(source: any = {}) {
	        return new UpdateSettingsInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.recordingDirectory = source["recordingDirectory"];
	        this.defaultQuality = source["defaultQuality"];
	        this.defaultSegmentMinutes = source["defaultSegmentMinutes"];
	        this.maxConcurrentRecordings = source["maxConcurrentRecordings"];
	        this.minimumFreeSpaceGiB = source["minimumFreeSpaceGiB"];
	        this.saveDisplayNames = source["saveDisplayNames"];
	    }
	}

}
