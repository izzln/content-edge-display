-- 播放项切换时淡出到黑、再淡入（由 display-agent 随 mpv 启动加载）。
--
-- mpv 不支持两个播放项之间的交叉溶解，这里改用 brightness 属性做"淡出到黑 → 淡入"：
-- brightness 是 GPU 渲染阶段的参数，开销可以忽略，不需要服务端为过渡重新编码；
-- 只作用于视频画面，模板叠加层（overlay-add 贴的属性区）不受影响，始终保持清晰。
--
-- 何时淡出：图片按 image-display-duration 计时，在结束前 FADE 秒开始；
-- 视频看 time-remaining，剩余不足 FADE 秒时开始。单张图片（时长 inf）不淡出。

local FADE = tonumber(mp.get_opt("display-fade") or "0.6") or 0.6
if FADE <= 0 then return end

local STEP = 1 / 30
local anim, fadeout_timer
local fading_out = false

local function stop_anim()
    if anim then anim:kill(); anim = nil end
end

-- 从当前亮度渐变到 target（-100 = 全黑，0 = 原样），smoothstep 曲线更自然。
local function ramp(target, dur)
    stop_anim()
    local from = mp.get_property_number("brightness", 0)
    local t0 = mp.get_time()
    anim = mp.add_periodic_timer(STEP, function()
        local p = (mp.get_time() - t0) / dur
        if p >= 1 then
            mp.set_property_number("brightness", target)
            stop_anim()
            return
        end
        p = p * p * (3 - 2 * p)
        mp.set_property_number("brightness", from + (target - from) * p)
    end)
end

local function fade_out()
    if fading_out then return end
    fading_out = true
    ramp(-100, FADE)
end

local function is_image()
    return mp.get_property_native("current-tracks/video/image") == true
end

mp.register_event("file-loaded", function()
    fading_out = false
    if fadeout_timer then fadeout_timer:kill(); fadeout_timer = nil end
    mp.set_property_number("brightness", -100)
    ramp(0, FADE)
    if is_image() then
        local dur = mp.get_property_number("image-display-duration")
        if dur and dur ~= math.huge and dur > 2 * FADE then
            fadeout_timer = mp.add_timeout(dur - FADE, fade_out)
        end
    end
end)

mp.observe_property("time-remaining", "number", function(_, remaining)
    if remaining and remaining <= FADE and remaining > 0 and not is_image() then
        fade_out()
    end
end)

mp.register_event("end-file", function()
    if fadeout_timer then fadeout_timer:kill(); fadeout_timer = nil end
end)
