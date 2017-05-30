import { Injectable, OnInit } from '@angular/core';
import { Http, Headers, RequestOptionsArgs, Response } from '@angular/http';
import { Location } from '@angular/common';
import { CookieService } from 'ngx-cookie';
import { Observable } from 'rxjs/Observable';
import { Subscriber } from 'rxjs/Subscriber';
import { BehaviorSubject } from 'rxjs/BehaviorSubject';

// Import RxJs required methods
import 'rxjs/add/operator/map';
import 'rxjs/add/operator/catch';
import 'rxjs/add/observable/throw';
import 'rxjs/add/operator/mergeMap';


import { XDSServerService, IXDSConfigProject } from "../services/xdsserver.service";
import { XDSAgentService } from "../services/xdsagent.service";
import { SyncthingService, ISyncThingProject, ISyncThingStatus } from "../services/syncthing.service";
import { AlertService, IAlert } from "../services/alert.service";
import { UtilsService } from "../services/utils.service";

export enum ProjectType {
    NATIVE = 1,
    SYNCTHING = 2
}

export interface INativeProject {
    // TODO
}

export interface IProject {
    id?: string;
    label: string;
    path: string;
    type: ProjectType;
    remotePrjDef?: INativeProject | ISyncThingProject;
    localPrjDef?: any;
    isExpanded?: boolean;
    visible?: boolean;
    defaultSdkID?: string;
}

export interface IXDSAgentConfig {
    URL: string;
    retry: number;
}

export interface ILocalSTConfig {
    ID: string;
    URL: string;
    retry: number;
    tilde: string;
}

export interface IConfig {
    xdsServerURL: string;
    xdsAgent: IXDSAgentConfig;
    xdsAgentZipUrl: string;
    projectsRootDir: string;
    projects: IProject[];
    localSThg: ILocalSTConfig;
}

@Injectable()
export class ConfigService {

    public conf: Observable<IConfig>;

    private confSubject: BehaviorSubject<IConfig>;
    private confStore: IConfig;
    private AgentConnectObs = null;
    private stConnectObs = null;
    private xdsAgentZipUrl = "";

    constructor(private _window: Window,
        private cookie: CookieService,
        private xdsServerSvr: XDSServerService,
        private xdsAgentSvr: XDSAgentService,
        private stSvr: SyncthingService,
        private alert: AlertService,
        private utils: UtilsService,
    ) {
        this.load();
        this.confSubject = <BehaviorSubject<IConfig>>new BehaviorSubject(this.confStore);
        this.conf = this.confSubject.asObservable();

        // force to load projects
        this.loadProjects();
    }

    // Load config
    load() {
        // Try to retrieve previous config from cookie
        let cookConf = this.cookie.getObject("xds-config");
        if (cookConf != null) {
            this.confStore = <IConfig>cookConf;
        } else {
            // Set default config
            this.confStore = {
                xdsServerURL: this._window.location.origin + '/api/v1',
                xdsAgent: {
                    URL: 'http://localhost:8000',
                    retry: 10,
                },
                xdsAgentZipUrl: "",
                projectsRootDir: "",
                projects: [],
                localSThg: {
                    ID: null,
                    URL: "http://localhost:8384",
                    retry: 10,    // 10 seconds
                    tilde: "",
                }
            };
        }

        // Update XDS Agent tarball url
        this.confStore.xdsAgentZipUrl = "";
        this.xdsServerSvr.getXdsAgentInfo().subscribe(nfo => {
            let os = this.utils.getOSName(true);
            let zurl = nfo.tarballs && nfo.tarballs.filter(elem => elem.os === os);
            if (zurl && zurl.length) {
                this.confStore.xdsAgentZipUrl = zurl[0].fileUrl;
                this.confSubject.next(Object.assign({}, this.confStore));
            }
        });
    }

    // Save config into cookie
    save() {
        // Notify subscribers
        this.confSubject.next(Object.assign({}, this.confStore));

        // Don't save projects in cookies (too big!)
        let cfg = Object.assign({}, this.confStore);
        delete (cfg.projects);
        this.cookie.putObject("xds-config", cfg);
    }

    loadProjects() {
        // Setup connection with local XDS agent
        if (this.AgentConnectObs) {
            try {
                this.AgentConnectObs.unsubscribe();
            } catch (err) { }
            this.AgentConnectObs = null;
        }

        let cfg = this.confStore.xdsAgent;
        this.AgentConnectObs = this.xdsAgentSvr.connect(cfg.retry, cfg.URL)
            .subscribe((sts) => {
                //console.log("Agent sts", sts);
                // FIXME: load projects from local XDS Agent and
                //  not directly from local syncthing
                this._loadProjectFromLocalST();

            }, error => {
                if (error.indexOf("XDS local Agent not responding") !== -1) {
                    let msg = "<span><strong>" + error + "<br></strong>";
                    msg += "You may need to download and execute XDS-Agent.<br>";
                    if (this.confStore.xdsAgentZipUrl !== "") {
                        msg += "<a class=\"fa fa-download\" href=\"" + this.confStore.xdsAgentZipUrl + "\" target=\"_blank\"></a>";
                        msg += " Download XDS-Agent tarball.";
                    }
                    msg += "</span>";
                    this.alert.error(msg);
                } else {
                    this.alert.error(error);
                }
            });
    }

    private _loadProjectFromLocalST() {
        // Remove previous subscriber if existing
        if (this.stConnectObs) {
            try {
                this.stConnectObs.unsubscribe();
            } catch (err) { }
            this.stConnectObs = null;
        }

        // FIXME: move this code and all logic about syncthing inside XDS Agent
        // Setup connection with local SyncThing
        let retry = this.confStore.localSThg.retry;
        let url = this.confStore.localSThg.URL;
        this.stConnectObs = this.stSvr.connect(retry, url).subscribe((sts) => {
            this.confStore.localSThg.ID = sts.ID;
            this.confStore.localSThg.tilde = sts.tilde;
            if (this.confStore.projectsRootDir === "") {
                this.confStore.projectsRootDir = sts.tilde;
            }

            // Rebuild projects definition from local and remote syncthing
            this.confStore.projects = [];

            this.xdsServerSvr.getProjects().subscribe(remotePrj => {
                this.stSvr.getProjects().subscribe(localPrj => {
                    remotePrj.forEach(rPrj => {
                        let lPrj = localPrj.filter(item => item.id === rPrj.id);
                        if (lPrj.length > 0) {
                            let pp: IProject = {
                                id: rPrj.id,
                                label: rPrj.label,
                                path: rPrj.path,
                                type: ProjectType.SYNCTHING,    // FIXME support other types
                                remotePrjDef: Object.assign({}, rPrj),
                                localPrjDef: Object.assign({}, lPrj[0]),
                            };
                            this.confStore.projects.push(pp);
                        }
                    });
                    this.confSubject.next(Object.assign({}, this.confStore));
                }), error => this.alert.error('Could not load initial state of local projects.');
            }), error => this.alert.error('Could not load initial state of remote projects.');

        }, error => {
            if (error.indexOf("Syncthing local daemon not responding") !== -1) {
                let msg = "<span><strong>" + error + "<br></strong>";
                msg += "Please check that local XDS-Agent is running.<br>";
                msg += "</span>";
                this.alert.error(msg);
            } else {
                this.alert.error(error);
            }
        });
    }

    set syncToolURL(url: string) {
        this.confStore.localSThg.URL = url;
        this.save();
    }

    set xdsAgentRetry(r: number) {
        this.confStore.localSThg.retry = r;
        this.confStore.xdsAgent.retry = r;
        this.save();
    }

    set xdsAgentUrl(url: string) {
        this.confStore.xdsAgent.URL = url;
        this.save();
    }


    set projectsRootDir(p: string) {
        if (p.charAt(0) === '~') {
            p = this.confStore.localSThg.tilde + p.substring(1);
        }
        this.confStore.projectsRootDir = p;
        this.save();
    }

    getLabelRootName(): string {
        let id = this.confStore.localSThg.ID;
        if (!id || id === "") {
            return null;
        }
        return id.slice(0, 15);
    }

    addProject(prj: IProject) {
        // Substitute tilde with to user home path
        prj.path = prj.path.trim();
        if (prj.path.charAt(0) === '~') {
            prj.path = this.confStore.localSThg.tilde + prj.path.substring(1);

            // Must be a full path (on Linux or Windows)
        } else if (!((prj.path.charAt(0) === '/') ||
            (prj.path.charAt(1) === ':' && (prj.path.charAt(2) === '\\' || prj.path.charAt(2) === '/')))) {
            prj.path = this.confStore.projectsRootDir + '/' + prj.path;
        }

        if (prj.id == null) {
            // FIXME - must be done on server side
            let prefix = this.getLabelRootName() || new Date().toISOString();
            let splath = prj.path.split('/');
            prj.id = prefix + "_" + splath[splath.length - 1];
        }

        if (this._getProjectIdx(prj.id) !== -1) {
            this.alert.warning("Project already exist (id=" + prj.id + ")", true);
            return;
        }

        // TODO - support others project types
        if (prj.type !== ProjectType.SYNCTHING) {
            this.alert.error('Project type not supported yet (type: ' + prj.type + ')');
            return;
        }

        let sdkPrj: IXDSConfigProject = {
            id: prj.id,
            label: prj.label,
            path: prj.path,
            hostSyncThingID: this.confStore.localSThg.ID,
            defaultSdkID: prj.defaultSdkID,
        };

        // Send config to XDS server
        let newPrj = prj;
        this.xdsServerSvr.addProject(sdkPrj)
            .subscribe(resStRemotePrj => {
                newPrj.remotePrjDef = resStRemotePrj;

                // FIXME REWORK local ST config
                //  move logic to server side tunneling-back by WS

                // Now setup local config
                let stLocPrj: ISyncThingProject = {
                    id: sdkPrj.id,
                    label: sdkPrj.label,
                    path: sdkPrj.path,
                    remoteSyncThingID: resStRemotePrj.builderSThgID
                };

                // Set local Syncthing config
                this.stSvr.addProject(stLocPrj)
                    .subscribe(resStLocalPrj => {
                        newPrj.localPrjDef = resStLocalPrj;

                        // FIXME: maybe reduce subject to only .project
                        //this.confSubject.next(Object.assign({}, this.confStore).project);
                        this.confStore.projects.push(Object.assign({}, newPrj));
                        this.confSubject.next(Object.assign({}, this.confStore));
                    },
                    err => {
                        this.alert.error("Configuration local ERROR: " + err);
                    });
            },
            err => {
                this.alert.error("Configuration remote ERROR: " + err);
            });
    }

    deleteProject(prj: IProject) {
        let idx = this._getProjectIdx(prj.id);
        if (idx === -1) {
            throw new Error("Invalid project id (id=" + prj.id + ")");
        }
        this.xdsServerSvr.deleteProject(prj.id)
            .subscribe(res => {
                this.stSvr.deleteProject(prj.id)
                    .subscribe(res => {
                        this.confStore.projects.splice(idx, 1);
                    }, err => {
                        this.alert.error("Delete local ERROR: " + err);
                    });
            }, err => {
                this.alert.error("Delete remote ERROR: " + err);
            });
    }

    private _getProjectIdx(id: string): number {
        return this.confStore.projects.findIndex((item) => item.id === id);
    }

}